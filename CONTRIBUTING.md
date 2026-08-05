# Contributing

Thanks for taking the time. Issues and pull requests are both welcome, and a
question filed as an issue is a fine contribution: it usually means something in
the README should have been clearer.

## Getting set up

You need Go 1.26+ and Docker. The end-to-end tests start real Postgres and Redis
through testcontainers, so nothing has to be running beforehand.

`make lint` needs **golangci-lint v2** — `.golangci.yml` uses the v2 schema, and
the v1 line cannot read it:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

```sh
cp .env.example .env    # set CONSOLE_PASSWORD
make test               # unit tests, no containers
make test-e2e           # the ones that need containers
```

## Before opening a pull request

```sh
make fmt
make lint
make test-cover
```

All three have to pass. `make test-cover` is the real gate: it merges coverage
from every test binary and fails below **100% of statements in `internal/`**.

That number is not decoration. It is why a bug in this stand is a bug you can
believe a test would have caught. If a new line is genuinely unreachable, say so
in the review rather than lowering the gate — and if a whole package cannot be
measured, add it to `MEASURED` in the `Makefile` *with the reason written down*,
the way the two existing exemptions are.

`check-measured` will also fail if a package produced no coverage data at all.
That is on purpose: a package no test binary imports would otherwise be quietly
left out of the total.

## Code

- **Layering.** `domain` knows nothing, `application` knows `domain`,
  `infrastructure` and `interfaces` know both. Nothing outside `kernel` is
  imported upward. `golangci-lint` enforces this — if it complains about an
  import, the fix is almost always to move the logic, not the import.
- **Comments explain why.** The existing code is commented heavily and
  deliberately: the *what* is in the code, so a comment that repeats it earns
  nothing. Match that. The SQL is commented the same way.
- **Protocol values stay raw.** Transaction states, error codes and millisecond
  timestamps are stored as the protocol sends them, not translated into names.
  A translation is one more thing to disbelieve when a rehearsal disagrees with
  production.
- **Error messages** carry all three languages (ru, uz, en), as the provider's
  own do.

## Schema changes

`migrations/` holds one squashed baseline. Add a new numbered goose file beside
it — do not edit the baseline, since anyone running the stand has already
applied it.

```sql
-- +goose Up
ALTER TABLE ...

-- +goose Down
ALTER TABLE ...
```

Wrap functions and anything else containing a semicolon in
`-- +goose StatementBegin` / `-- +goose StatementEnd`.

## Commits and pull requests

One concern per pull request. Describe what was broken or missing and how you
would tell that it is fixed — a test usually says both at once.

## Reporting a bug

Say what you called, what came back, and what you expected instead. The traffic
log in the console records both bodies and both header sets for every call; the
relevant entry is usually the whole bug report.

If it is a **security** issue, do not open a public issue — see
[SECURITY.md](SECURITY.md).

## License

Contributions are accepted under the [MIT License](LICENSE).
