# review-system

Go module for the PR review automation. Delivery rows are tracked in
`pilot/session-register.json`. The protocol is in `docs/comparison-protocol.md`.

Status by row (delivery stage 1.1, M1):

| Row | Package | Status |
|---|---|---|
| W1.1.1 | `pilot` | Profile schema, session register, protocol doc. Implemented, tested |
| W1.1.2 | `bundle` | Frozen bundle export from a local git clone. Implemented, tested |
| W1.1.6 | `card` | Decision card and operator time form. Implemented, tested |
| W1.1.3, W1.1.4 | – | Blocked: needs `G-RUN` (a verified Loom execution path). Not built |
| W1.1.5, W1.1.7 | – | Blocked by W1.1.3 and W1.1.4 |

Not built: GitHub-backed source (only the local git source exists), the review
runners, the publisher, the cockpit and Lua collection. Nothing here writes to GitHub.

Run the tests from this directory:

```bash
go test -tags fts5 -race -count=1 ./...
```
