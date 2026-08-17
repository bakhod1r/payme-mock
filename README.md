# payme-mock

[![CI](https://github.com/bakhod1r/payme-mock/actions/workflows/ci.yml/badge.svg)](https://github.com/bakhod1r/payme-mock/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/badge/internal%20coverage-100%25-brightgreen)](#development)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

A local stand for integrating against [Payme](https://developer.help.paycom.uz/).
It speaks both halves of the protocol — the **Subscribe API** your backend
calls, and the **Merchant API** the provider calls back — so an integration can
be driven end to end without a real cashbox, a real card, or a network.

It is not a stub. Payments hold state, cards can be rigged to fail the way real
cards fail, and every call in and out is recorded with both bodies and both
header sets, so a failing integration is read rather than guessed at.

**[Try the API in your browser](https://bakhod1r.github.io/payme-mock/playground/)** —
the Subscribe API compiled to WebAssembly, running in the page with no server
behind it. Or **[browse the console](https://bakhod1r.github.io/payme-mock/demo/)**,
a static snapshot of the admin UI after a run.

> **Not affiliated with Payme or Paycom.** This is an independent testing tool
> built from the public protocol documentation.

---

## What it is for

Three things are hard to rehearse against a real sandbox:

- **Failures on demand.** A blocked card, an expired card, a card the
  processing side rejects, a ten-second timeout, a malformed response, a
  dropped connection, a webhook delivered twice. Here each is a rule you switch
  on.
- **The other side's view.** The stand keeps the provider's record of a
  transaction *separately* from the merchant's. When the two disagree, that
  disagreement is visible — which is the bug class this tool exists for.
- **Repeatability.** One seed file puts the whole stand back to a known state,
  so a run means the same thing tomorrow.

## Services

| Service     | Port   | What it is |
|-------------|--------|------------|
| `paymemock` | `8082` | The provider side: the Subscribe API server, the checkout emulator, and the caller of your Merchant API. Nothing it returns says it is a mock. |
| `merchant`  | `8081` | An example merchant: the Merchant API endpoint the provider calls, with payers, orders and transactions behind it. Useful on its own, and the reference for what your own endpoint has to do. |
| `console`   | `8080` | The control UI: sandboxes, cards, payments, fault rules, the IP allowlist and the live traffic log. |

Two more are designed and configured but **not yet written** — `worker`
(background state walks: holds expiring, transactions timing out) and `gateway`
(HTTPS front door with a stable endpoint URL per sandbox). Their blocks in
`docker-compose.yml` are commented out.

## Quick start

```sh
git clone https://github.com/bakhod1r/payme-mock.git
cd payme-mock

cp .env.example .env
# The console shows every sandbox's keys, so it refuses to start without a
# password. There is no default.
$EDITOR .env          # set CONSOLE_PASSWORD

make up               # docker compose up -d --build
```

The console is on <http://127.0.0.1:8080>, bound to loopback deliberately: it
shows every sandbox's keys and skips the login for local callers.

Load the example integration — three cashboxes, a payer each, and the
provider's published test cards:

```sh
docker compose exec -T postgres psql -U payme -d paymemock < seed/example_cashboxes.sql
```

Then point your backend's Subscribe base URL at `http://localhost:8082/api` and
give it a sandbox's merchant id and test key from the console. Nothing else in
your code changes: swapping the base URL back to
`https://checkout.test.paycom.uz/api` runs the same code against the real
provider.

## Sandboxes

A sandbox is one cashbox: a merchant id, a live key and a test key. Everything
else — cards, payments, traffic, fault rules — hangs off it, so two people can
share one stand without seeing each other's runs.

Sandboxes carry a `kind` (`topup`, `deposit`, `dividend`), because the Subscribe
API behaves differently for a payout cashbox than for a top-up, and a
`merchant_group`. Cashboxes naming the same group share their cards, the way
the provider holds one card per merchant and hands out a token per cashbox.

## Cards

Cards are added through the Subscribe API or in the console, and each carries an
`outcome` that decides what happens when it is used:

| Outcome | What the caller sees |
|---|---|
| `success` | The payment goes through. |
| `insufficient_funds` | Declined for money. |
| `blocked` | Card blocked. |
| `expired` | Card expired. |
| `verify_failed` | The OTP is never accepted. |
| `system_error` | An unattributed processing failure. |

Independently of the outcome: `delay_ms` holds the answer back, `sms_enabled`
withholds the OTP so the verification path can be tested without one, and
`frozen` keeps a payment from ever settling.

The stand's shared OTP is **`666666`**. A card an integration tokenized for
itself falls back to its own expiry instead — `03/99` takes `039999`.

## Fault injection

A fault rule matches on service, method (globs allowed: `receipts.*`,
`*Transaction`), the account object, the transaction id and an amount range,
and then does one thing:

`delay` · `rpc_error` · `http_status` · `malformed` · `drop` · `duplicate` ·
`passthrough`

Rules are ordered by priority and the first match wins, so a broad rule can sit
behind a narrow one. `probability` and `times_left` make a rule intermittent,
which is what a retry path actually has to survive. Every call answered by a
rule is marked as such in the traffic log, so a deliberate failure is never
mistaken for a real one.

Rules can be bundled into a named **config** and switched onto a sandbox as a
unit, so a scenario is set up once and replayed rather than rebuilt.

There is also a per-sandbox **IP allowlist**: the real provider calls a merchant
only from its own addresses, and an integration that never rehearses being
refused discovers that in production.

## Configuration

Everything is environment variables; `.env.example` documents each one with the
reason it exists. The essentials:

| Variable | Meaning |
|---|---|
| `CONSOLE_USER`, `CONSOLE_PASSWORD` | Console credentials. No default for the password. |
| `DATABASE_URL` | Postgres. Host ports are shifted (`5433`, `6380`) so the stand does not collide with a local server. |
| `REDIS_ADDR` | Redis. |
| `HTTP_ADDR` | Per service — each reads the same name, so set it per service, not globally. |
| `SUBSCRIBE_BASE_URL` | Where the merchant sends Subscribe API calls. |
| `MERCHANT_BASE_URL` | Where the emulator sends Merchant API webhooks. |
| `DB_MIGRATE_ON_START` | Turn off when a deployment migrates separately. |

Configuration is read from `.env`, then `.env.local` (never committed), then the
files for the active `APP_ENV`. All are optional — in a container the whole
configuration arrives as environment variables instead.

## Development

```sh
make help          # every target
make test          # unit tests, no containers
make test-e2e      # tests that start real Postgres and Redis via testcontainers
make test-cover    # the full suite behind the coverage gate
make lint          # golangci-lint, including the layer boundary rules
make fmt
```

`internal/` is held at **100% statement coverage** and CI fails below it. The
gate is deliberately more than a percentage: `check-measured` fails if a
measured package produced no coverage data at all, because a package no test
binary imports is otherwise silently excluded from the total.

Two packages are exempt with a reason recorded in the `Makefile`:
`postgres/testdb` exists only to start containers for other tests, and
`traffic/domain` declares types and nothing else.

## Layout

```
cmd/           one main per service
internal/
  context/     the domains, each split into
    payment/     domain · application · infrastructure · interfaces
    simulation/
  kernel/      what every context may use: config, clock, errors,
               HTTP middleware, JSON-RPC, Postgres
migrations/    the schema, embedded into every binary
seed/          example data for one integration
web/console/   the console's templates
```

The dependency rule is one way: `domain` knows nothing, `application` knows
`domain`, `infrastructure` and `interfaces` know both, and nothing outside
`kernel` is imported upward. The linter enforces it.

The database is three schemas — `merchant`, `mock`, `control` — because the two
payment sides hold tables of the same name on purpose. Each keeps its own
record of a transaction, and a rehearsal is worth running precisely when the
two disagree.

## The published site

<https://bakhod1r.github.io/payme-mock/> is a landing page, a live playground
under `/playground/`, and a static snapshot of the console under `/demo/`. All
of it lives in `docs/` and is deployed by `.github/workflows/pages.yml`.

### The playground

`cmd/playground` compiles the Subscribe API to WebAssembly and answers it inside
the page. It is worth saying why that is possible at all: the domain has no
database in it and the application layer talks only to the ports the domain
declares, so swapping Postgres for maps is a wiring change rather than a
rewrite. The in-memory ports are in
`internal/context/payment/subscribe/infrastructure/inmem`.

```sh
make playground     # build the wasm and copy Go's loader beside it
```

What it is not is the whole stand. There is no merchant behind it, so the
Merchant API chain is accepted rather than driven; no worker, so a receipt
settles at once; no database, so a reload starts over. The console is not in it
either — its store is raw SQL rather than ports, so it has nothing to swap.

The snapshot is generated, not written. To refresh it:

```sh
make up
docker compose exec -T postgres psql -q -U payme -d paymemock < seed/example_cashboxes.sql
scripts/demo-data.sh                        # drive real traffic through it
python3 scripts/snapshot-console.py docs/demo
```

`demo-data.sh` makes the stand worth looking at: settled top-ups across all
three cashboxes, withdrawals back onto cards, the rigged cards refusing as they
are meant to, payments left in progress and cancelled, calls refused for a bad
key, and a set of fault and allowlist rules. Every row comes from a real call,
so the traffic log and the merchant's own books agree the way they would after a
rehearsal.

The snapshot crawler then fetches each screen and rewrites it for a static host:
links become sibling `.html` files, and each filter is captured as its own page
so the controls still work. Anything that would change something — every POST
form and its buttons — is made inert.

## Deploying

Running the stand on a server rather than a laptop — Compose on one host, a
proxy in front, backups and updates — is covered in [DEPLOY.md](DEPLOY.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Issues and pull requests are welcome.

## License

[MIT](LICENSE).
