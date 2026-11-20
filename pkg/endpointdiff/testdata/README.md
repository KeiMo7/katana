# Two-week endpoint diff sample

These fixtures are a compact, self-contained version of the two-crawl
scenario exercised by the end-to-end test. They are ordinary katana jsonl
(`-j -kb-endpoints`) inventories plus the crawl-scope manifests written by
`-crawl-manifest`.

## What changes between the two weeks

| Endpoint (identity view) | Week 1 | Week 2 | Classification |
| --- | --- | --- | --- |
| `GET /api/v1/users/{num}` | `/users/1001` | `/users/2001`, `/users/2002?page&sort` | **persisted**, numeric id normalized, `sort` is a parameter change |
| `GET /api/v1/orders` (`limit`) | present | absent | **removed** (same crawl scope) |
| `GET /api/v1/legacy-reports` | present | absent | **removed** |
| `GET /api/v1/fresh-metrics` | absent | present | **added** |
| `GET /api/v1/profile/me` | present, source `page-about.html` | present, same source | **persisted**, source page retained to the report |

The first line of each jsonl is a plain HTML page with no
`knowledgebase.endpoints` payload: it must be ignored and counted, never
fail the parse.

`TestTestdata_SampleReport` in `testdata_test.go` diffs the two inventories
and compares the result byte-for-byte with `expected.diff.json`, so the
sample cannot silently drift from the implementation.
