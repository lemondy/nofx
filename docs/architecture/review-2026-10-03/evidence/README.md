# Review evidence · 2026-10-03

Reviewed revision: `e68ed1fd9110`.

All new tests assert the safe result, and intentionally fail on this revision.
They use fake trading interfaces or temporary SQLite/jsdom, with no real
exchange calls, API writes, credentials, or production state changes.

From the repository root:

```sh
python3 docs/architecture/review-2026-10-03/evidence/reproduce.py
python3 docs/architecture/review-2026-10-03/evidence/reproduce_web.py
```

The Go runner adds virtual test files using `go test -overlay`. The web runner
temporarily copies a test beside the actual page, executes Vitest, and removes
it in `finally`. It refuses to replace an existing file. Dependencies must
already be installed; the runners do not install or change packages.

The normal Go/Vitest suite, Go vet, selected Go race suite, and frontend build
pass. Frontend lint fails predominantly on formatting. The regression logs
show 14 Go safety assertion failures and 2 page interaction assertion failures;
these are different from the passing normal test suite. See the main report
for conditions and limitations, especially quantity-sized protective orders,
partial-fill semantics, and static-only findings.

Sandboxed environments may require permission for Go's existing build cache
and local mock HTTP ports. The macOS linker warnings in the race log did not
fail that run.

The original 11 MB lint output is condensed into its non-format diagnostics,
first formatting examples, final counts, and original SHA256. Re-run the lint
command to regenerate the complete output.
