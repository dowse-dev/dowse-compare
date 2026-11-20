# dowse-compare

The app behind [compare.dowse.dev](https://compare.dowse.dev): one query goes to seven search APIs at once — Dowse (basic and advanced), Exa, Tavily, Perplexity, Context.dev, Parallel and Keenable — and an AI judge grades every result without knowing which API returned it.

Every API, Dowse included, is called through its **public API with an API key**. Nothing here has inside access to any of them.

## How a run is scored

1. **Same query, 10 results each.** Every API gets the query at the same time, with its default settings: Dowse `basic` and `advanced`, Exa `auto`, Tavily `basic`, the Perplexity Search API, Context.dev web search, Parallel (default `advanced`) and Keenable (default `pro`). Every API gets only the query. Parallel's docs suggest also writing a goal and 2–3 keyword queries; no API here is given extra help, so Parallel gets the query as its objective and its one search query.
2. **One blind pool.** All results go into one list. Copies of one page are merged (`http`/`https`, `www.`, a leading language path such as `/en-us/`), the order is shuffled and the API names are removed.
3. **The judge.** Jev, TypeSafe's System One model on [OpenRouter](https://openrouter.ai), answers for each pool result: relevance 0–3 with a confidence, and the probability it is spam or clickbait. It is told today's date. The prompts are in [`internal/judge`](internal/judge/judge.go). Dowse also uses Jev, with a different prompt, to rank its own results, which is why the judge never sees which API returned what.
4. **Grade per result:** `min(1, relevance/3) × (0.7 + 0.3 × confidence) × max(0.1, 1 − 0.75 × spam)`, shown × 100.
5. **Score per API (nDCG@10):** the DCG of its own 10 grades in its own order, divided by the DCG of the best 10 grades in the whole pool, × 100. `DCG = Σ grade / log2(position + 1)`, so #1 counts about 3.5 times as much as #10, and missing a strong page another API found costs points.
6. **Ties:** the judge varies a little between runs, so the top column gets the *Best* badge only when it leads every other company's best column by 2 points or more.
7. **Price:** each API's list price per query at its entry paid tier; Dowse, Exa and Perplexity as their APIs report it.

Rate limits: free tiers allow only a few requests per second or minute. When a provider answers 429, that key rests a few seconds and the call moves to the next key, or waits up to 10 seconds for one; if none comes back, the column says the API is busy.

## Check a run yourself

Every run has a JSON record (`/api/compare/run?id=N&download=1`, linked under each run) with every result, every grade and the exact request sent to the judge. Send that request with your own OpenRouter key and compare:

```sh
jq '.run.judge.request' dowse-compare-run-N.json \
  | curl -s https://openrouter.ai/api/v1/systemone \
      -H "Authorization: Bearer $OPENROUTER_API_KEY" \
      -H 'Content-Type: application/json' -d @-
```

On one run of 26 results the replay gave the same relevance on all 26 and grades within 0.05 (mean difference 0.011).

## Run it

```sh
cp .env.example .env   # fill in the keys
go run .               # http://127.0.0.1:8130
```

Requests from the machine itself skip the per-IP limit and the human check, so local testing is easy. Each fresh comparison spends one call per provider; finished runs are kept in SQLite (`data/compare.db`) and repeats of a query within 24 hours are answered from there.

## Layout

| File | What |
|---|---|
| `compare.go` | providers, the stream, the blind pool, the judge call, nDCG scoring |
| `compare_extract.go` | the Extract tab: one URL to five extract APIs, yes/no checks by the judge |
| `compare_keys.go` | several keys per provider, moving to the next when one runs out |
| `compare_store.go` | saved runs, the public list, downloads |
| `dowse.go` | Dowse's public API |
| `internal/judge` | the judge: its prompts and the System One client (OpenRouter) |
| `web/compare.html` | the page |

## License

MIT. See [LICENSE](LICENSE).
