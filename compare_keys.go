package main

// Each provider on the comparison page can have several free-tier keys:
// NAME, NAME_2, NAME_3 ... in the settings. Calls take the keys in turn
// (round robin), skipping any key that is resting. When a provider says a key is out of credit, invalid or over
// its rate limit, that key rests and the same call is retried at once with
// the next key, so a visitor never sees the switch. Free tiers reset monthly,
// so a key that ran out is simply tried again after its rest.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	compareKeyRestQuota = 6 * time.Hour    // out of credit or rejected: try again later
	compareKeyRestRate  = 5 * time.Second  // rate limited (429): free tiers allow a few requests a second or minute
	compareKeyMaxWait   = 10 * time.Second // the longest a call waits for a rate-limited key to come back
	compareMaxKeys      = 9
)

var (
	// errCompareNoKeyLeft: every key of a provider is out of credit or rejected.
	errCompareNoKeyLeft = errors.New("every key is out of quota")
	// errCompareBusy: every key is rate limited and none came back in time.
	errCompareBusy = errors.New("every key is rate limited")
)

// compareHTTPError is a provider's non-200 answer. Body is for the log only:
// it can carry account details, so it never reaches the page.
type compareHTTPError struct {
	Status int
	Body   string
}

func (e *compareHTTPError) Error() string {
	return fmt.Sprintf("status %d: %s", e.Status, e.Body)
}

// compareKeyPool is one provider's keys and when each may be used again.
type compareKeyPool struct {
	name  string
	keys  []string
	mu    sync.Mutex
	until []time.Time
	rated []bool // the key rests because of a rate limit, not its credit
	turn  int    // round robin: the key the next call starts from
	now   func() time.Time
	sleep func(context.Context, time.Duration) bool // waits; false when ctx ends first
}

// compareKeysFromEnv returns base's key (first, from the loaded config) and
// base_2 ... base_9 from the environment, skipping empty ones and repeats.
func compareKeysFromEnv(base, first string) []string {
	var keys []string
	seen := map[string]bool{}
	add := func(k string) {
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	add(first)
	for i := 2; i <= compareMaxKeys; i++ {
		add(os.Getenv(fmt.Sprintf("%s_%d", base, i)))
	}
	return keys
}

func newCompareKeyPool(name string, keys []string) *compareKeyPool {
	return &compareKeyPool{name: name, keys: keys, until: make([]time.Time, len(keys)), rated: make([]bool, len(keys)), now: time.Now, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// empty reports whether the provider has no key at all (its column shows "Not connected").
func (p *compareKeyPool) empty() bool { return p == nil || len(p.keys) == 0 }

// next is the first key not resting, or -1.
// next is the usable key whose turn it is, or -1. Calls take turns (key 1,
// then 2, then 3, then 1 again), so the providers' per-key rate and
// concurrency limits are shared across every key; a resting key is skipped.
func (p *compareKeyPool) next() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	n := len(p.keys)
	for k := 0; k < n; k++ {
		i := (p.turn + k) % n
		if !now.Before(p.until[i]) {
			p.turn = (i + 1) % n
			return i
		}
	}
	return -1
}

func (p *compareKeyPool) rest(i int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.now().Add(d); t.After(p.until[i]) {
		p.until[i] = t
		p.rated[i] = d == compareKeyRestRate
	}
}

// soonest is how long until the first rate-limited key may be used again, and
// whether any key rests only for a rate limit (false: all are out of credit).
func (p *compareKeyPool) soonest() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now, best, any := p.now(), time.Duration(0), false
	for i := range p.keys {
		if !p.rated[i] {
			continue
		}
		d := p.until[i].Sub(now)
		if !any || d < best {
			best, any = d, true
		}
	}
	return best, any
}

// keyRest says how long a key must rest after err, or 0 when err is not about the key.
func keyRest(err error) time.Duration {
	var he *compareHTTPError
	if !errors.As(err, &he) {
		return 0
	}
	switch he.Status {
	case 429:
		return compareKeyRestRate
	case 401, 402, 403, 432, 433:
		// 401 USAGE_EXCEEDED (Context.dev), 432/433 plan or pay-as-you-go limit (Tavily),
		// 402 no credit, 401/403 bad or disabled key.
		return compareKeyRestQuota
	}
	return 0
}

// do runs call with each usable key in turn until one gets past the key
// checks. When every key is resting and some only for a rate limit, it waits
// for the first to come back (up to compareKeyMaxWait in all) and tries again.
func (p *compareKeyPool) do(ctx context.Context, call func(key string) error) error {
	if p.empty() {
		return errCompareUnavailable
	}
	waited := time.Duration(0)
	for attempt := 0; attempt < 3*len(p.keys)+3; attempt++ {
		i := p.next()
		if i < 0 {
			d, rated := p.soonest()
			if !rated {
				return errCompareNoKeyLeft
			}
			if d < 0 {
				d = 0
			}
			if waited+d > compareKeyMaxWait || !p.sleep(ctx, d+50*time.Millisecond) {
				return errCompareBusy
			}
			waited += d
			continue
		}
		err := call(p.keys[i])
		d := keyRest(err)
		if d == 0 {
			return err
		}
		p.rest(i, d)
		log.Printf("compare: %s key %d of %d resting %s: %v", p.name, i+1, len(p.keys), d, err)
	}
	return errCompareBusy
}

// status is how many keys the provider has and how many are usable now, for /health-style checks.
func (p *compareKeyPool) status() (total, usable int) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for i := range p.keys {
		if !now.Before(p.until[i]) {
			usable++
		}
	}
	return len(p.keys), usable
}
