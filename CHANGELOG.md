# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Nothing yet.

## [1.0.0] - 2026-08-05

First stable release. The stand speaks both halves of the Payme protocol and can
drive an integration end to end without a real cashbox, card, or network.

### Added

#### Services

- `paymemock` (`:8082`) — the provider side: Subscribe API server, checkout
  emulator, and the caller of your Merchant API. Nothing it returns identifies
  itself as a mock.
- `merchant` (`:8081`) — an example merchant: the Merchant API endpoint the
  provider calls, with payers, orders and transactions behind it.
- `console` (`:8080`) — control UI for sandboxes, cards, payments, fault rules,
  the IP allowlist and the live traffic log.

#### Sandboxes

- A sandbox is one cashbox: merchant id, live key, test key. Cards, payments,
  traffic and fault rules hang off it, so two people can share one stand without
  seeing each other's runs.
- Sandbox `kind` (`topup`, `deposit`, `dividend`) — the Subscribe API behaves
  differently for a payout cashbox than for a top-up.
- `merchant_group` — cashboxes naming the same group share their cards, the way
  the provider holds one card per merchant and hands out a token per cashbox.

#### Cards

- Cards added through the Subscribe API or the console, each carrying an
  `outcome`: `success`, `insufficient_funds`, `blocked`, `expired`,
  `verify_failed`, `system_error`.
- Independent modifiers: `delay_ms` (hold the answer back), `sms_enabled`
  (withhold the OTP), `frozen` (never settle).
- Shared stand OTP `666666`; self-tokenized cards fall back to their own expiry
  (`03/99` takes `039999`).

#### Fault injection

- Rules match on service, method (globs: `receipts.*`, `*Transaction`), account
  object, transaction id and amount range.
- Actions: `delay`, `rpc_error`, `http_status`, `malformed`, `drop`,
  `duplicate`, `passthrough`.
- Priority ordering with first-match-wins; `probability` and `times_left` make a
  rule intermittent, which is what a retry path actually has to survive.
- Every call answered by a rule is marked as such in the traffic log, so a
  deliberate failure is never mistaken for a real one.
- Rules bundle into named **configs**, switchable onto a sandbox as a unit.
- Per-sandbox **IP allowlist**, so being refused can be rehearsed.

#### Observability

- Full traffic log: every call in and out recorded with both bodies and both
  header sets.

#### Published site

- Browser **playground** — the Subscribe API compiled to WebAssembly, running in
  the page with no server behind it.
- Static **console demo** snapshot of the admin UI after a run.

#### Project

- Docker and `docker-compose` setup, `Makefile`, CI and Pages workflows.
- MIT license, contribution guide, security policy, issue templates and
  Dependabot configuration.

### Not yet implemented

- `worker` — background state walks (holds expiring, transactions timing out).
- `gateway` — HTTPS front door with a stable endpoint URL per sandbox.

Both are designed and configured; their `docker-compose.yml` blocks are
commented out.

### Note

Not affiliated with Payme or Paycom. Independent testing tool built from the
public protocol documentation.

[Unreleased]: https://github.com/bakhod1r/payme-mock/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/bakhod1r/payme-mock/releases/tag/v1.0.0
