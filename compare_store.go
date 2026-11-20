package main

// compareStore keeps every finished comparison in SQLite. The page lists them
// publicly (newest first) and anyone can open one, so a run is paid for once
// and then shown for free. It also serves as the 24-hour query cache. Nothing
// about who ran a query is stored.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type compareStore struct {
	db *sql.DB
}

// Kinds of stored run.
const (
	compareKindSearch  = "search"
	compareKindExtract = "extract"
)

// compareHistoryItem is one row of the public list. Search runs carry the
// winner and scores; extract runs the question and the providers that passed.
type compareHistoryItem struct {
	ID       int64              `json:"id"`
	Kind     string             `json:"kind"`
	Query    string             `json:"query"` // the query, or the URL of an extract run
	Question string             `json:"question,omitempty"`
	At       time.Time          `json:"at"`
	Winner   string             `json:"winner"`
	Scores   map[string]float64 `json:"scores,omitempty"`
	Passed   []string           `json:"passed,omitempty"`
	Total    int                `json:"total,omitempty"` // providers that answered
}

func openCompareStore(path string) (*compareStore, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create compare db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS compare_runs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	qkey       TEXT    NOT NULL,
	query      TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	winner     TEXT    NOT NULL DEFAULT '',
	run        TEXT    NOT NULL,
	hidden     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS compare_runs_qkey ON compare_runs(qkey, created_at);
CREATE INDEX IF NOT EXISTS compare_runs_created ON compare_runs(created_at);`)
	if err == nil {
		err = addColumnIfMissing(db, "compare_runs", "kind", "TEXT NOT NULL DEFAULT 'search'")
	}
	if err == nil {
		err = recheckWinners(db)
	}
	if err == nil {
		err = dropOldJudgeRequests(db)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("compare db schema: %w", err)
	}
	return &compareStore{db: db}, nil
}

func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	rows.Close()
	_, err = db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

func (s *compareStore) Close() error { return s.db.Close() }

// save stores a finished run and sets its ID and time.
func (s *compareStore) save(run *compareRun, at time.Time) error {
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	winner := ""
	if run.Verdict != nil {
		winner = run.Verdict.Winner
	}
	id, err := s.insert(compareKindSearch, compareCacheKey(run.Query), run.Query, winner, b, at)
	if err != nil {
		return err
	}
	run.ID, run.At = id, at.UTC().Truncate(time.Second)
	return nil
}

func (s *compareStore) insert(kind, qkey, query, winner string, run []byte, at time.Time) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO compare_runs (kind, qkey, query, created_at, winner, run) VALUES (?, ?, ?, ?, ?, ?)`,
		kind, qkey, query, at.Unix(), winner, string(run))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// saveExtract stores a finished extract run under key and sets its ID and time.
func (s *compareStore) saveExtract(run *compareExtractRun, key string, at time.Time) error {
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	id, err := s.insert(compareKindExtract, key, run.URL, "", b, at)
	if err != nil {
		return err
	}
	run.ID, run.At = id, at.UTC().Truncate(time.Second)
	return nil
}

// latestExtract returns the newest visible extract run for key made at or after since, or nil.
func (s *compareStore) latestExtract(key string, since time.Time) (*compareExtractRun, error) {
	var id, at int64
	var raw string
	err := s.db.QueryRow(`SELECT id, created_at, run FROM compare_runs WHERE kind = ? AND qkey = ? AND created_at >= ? AND hidden = 0 ORDER BY created_at DESC, id DESC LIMIT 1`,
		compareKindExtract, key, since.Unix()).Scan(&id, &at, &raw)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var run compareExtractRun
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		return nil, err
	}
	run.ID, run.At = id, time.Unix(at, 0).UTC()
	return &run, nil
}

// latest returns the newest visible run of the query made at or after since, or nil.
func (s *compareStore) latest(query string, since time.Time) (*compareRun, error) {
	row := s.db.QueryRow(`SELECT id, created_at, run FROM compare_runs WHERE kind = ? AND qkey = ? AND created_at >= ? AND hidden = 0 ORDER BY created_at DESC, id DESC LIMIT 1`,
		compareKindSearch, compareCacheKey(query), since.Unix())
	return scanCompareRun(row)
}

// get returns a visible run's kind and JSON (with its id and time set), or a
// nil JSON when there is none.
func (s *compareStore) get(id int64) (string, json.RawMessage, error) {
	var kind, raw string
	var at int64
	err := s.db.QueryRow(`SELECT kind, created_at, run FROM compare_runs WHERE id = ? AND hidden = 0`, id).Scan(&kind, &at, &raw)
	if err == sql.ErrNoRows {
		return "", nil, nil
	} else if err != nil {
		return "", nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", nil, err
	}
	m["id"], m["at"] = id, time.Unix(at, 0).UTC()
	b, err := json.Marshal(m)
	return kind, b, err
}

func scanCompareRun(row *sql.Row) (*compareRun, error) {
	var id, at int64
	var raw string
	if err := row.Scan(&id, &at, &raw); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var run compareRun
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		return nil, err
	}
	run.ID, run.At = id, time.Unix(at, 0).UTC()
	return &run, nil
}

// list returns the newest visible runs, each query once (its newest run).
func (s *compareStore) list(limit int) ([]compareHistoryItem, error) {
	rows, err := s.db.Query(`
SELECT id, kind, query, created_at, winner, run FROM compare_runs
WHERE hidden = 0 AND id IN (SELECT MAX(id) FROM compare_runs WHERE hidden = 0 GROUP BY kind, qkey)
ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []compareHistoryItem{}
	for rows.Next() {
		var it compareHistoryItem
		var at int64
		var raw string
		if err := rows.Scan(&it.ID, &it.Kind, &it.Query, &at, &it.Winner, &raw); err != nil {
			return nil, err
		}
		it.At = time.Unix(at, 0).UTC()
		if it.Kind == compareKindExtract {
			var run compareExtractRun
			if json.Unmarshal([]byte(raw), &run) == nil {
				it.Question = run.Question
				for _, c := range run.Columns {
					if c.Status == "unavailable" {
						continue
					}
					it.Total++
					if run.Verdict == nil {
						continue
					}
					if ch, ok := run.Verdict.Checks[c.ID]; ok && ch.Real && (ch.Answers == nil || *ch.Answers) {
						it.Passed = append(it.Passed, c.ID)
					}
				}
			}
			out = append(out, it)
			continue
		}
		var run compareRun
		if json.Unmarshal([]byte(raw), &run) == nil && run.Verdict != nil {
			it.Scores = map[string]float64{}
			for id, sc := range run.Verdict.Scores {
				if sc.Score != nil {
					it.Scores[id] = *sc.Score
				}
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// recheckWinners applies the current winner rule (compareWinner) to every
// stored search run, so a rule fix reaches old runs too. It only rewrites
// rows whose winner changes.
func recheckWinners(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, winner, run FROM compare_runs WHERE kind = 'search'`)
	if err != nil {
		return err
	}
	type fix struct {
		id     int64
		winner string
		run    []byte
	}
	var fixes []fix
	for rows.Next() {
		var id int64
		var winner, raw string
		if err := rows.Scan(&id, &winner, &raw); err != nil {
			rows.Close()
			return err
		}
		var run map[string]json.RawMessage
		var v compareVerdict
		if json.Unmarshal([]byte(raw), &run) != nil || json.Unmarshal(run["verdict"], &v) != nil || v.Scores == nil {
			continue
		}
		scores := map[string]float64{}
		for pid, cs := range v.Scores {
			if cs.Score != nil {
				scores[pid] = *cs.Score
			}
		}
		w := compareWinner(scores)
		if w == winner && w == v.Winner {
			continue
		}
		v.Winner = w
		vb, _ := json.Marshal(v)
		run["verdict"] = vb
		rb, err := json.Marshal(run)
		if err != nil {
			continue
		}
		fixes = append(fixes, fix{id, w, rb})
	}
	rows.Close()
	for _, f := range fixes {
		if _, err := db.Exec(`UPDATE compare_runs SET winner = ?, run = ? WHERE id = ?`, f.winner, string(f.run), f.id); err != nil {
			return err
		}
	}
	if len(fixes) > 0 {
		log.Printf("compare: winner rule re-applied to %d stored run(s)", len(fixes))
	}
	return nil
}

// oldJudgeMarker is a line only the first judge's requests have. Those runs
// keep their grades but lose the request: it carried a prompt that is not
// published.
const oldJudgeMarker = "Scoring instructions (apply to each result below)"

func dropOldJudgeRequests(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, run FROM compare_runs WHERE kind = 'search' AND instr(run, ?) > 0`, oldJudgeMarker)
	if err != nil {
		return err
	}
	type fix struct {
		id  int64
		run []byte
	}
	var fixes []fix
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var run map[string]json.RawMessage
		var j map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &run) != nil || json.Unmarshal(run["judge"], &j) != nil {
			continue
		}
		delete(j, "request")
		jb, _ := json.Marshal(j)
		run["judge"] = jb
		rb, err := json.Marshal(run)
		if err == nil {
			fixes = append(fixes, fix{id, rb})
		}
	}
	rows.Close()
	for _, f := range fixes {
		if _, err := db.Exec(`UPDATE compare_runs SET run = ? WHERE id = ?`, string(f.run), f.id); err != nil {
			return err
		}
	}
	if len(fixes) > 0 {
		log.Printf("compare: removed the first judge's request from %d stored run(s)", len(fixes))
	}
	return nil
}
