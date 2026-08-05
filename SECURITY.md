# Security

## Reporting a vulnerability

Please do not open a public issue. Report it privately through GitHub's
[security advisory form](https://github.com/bakhod1r/payme-mock/security/advisories/new).

Include what you called, what happened, and what you expected. You will get an
acknowledgement within a few days.

## What this project is

payme-mock is a **testing stand**. It is meant to run on a developer machine or
inside a private network, and it is deliberately permissive there:

- The console shows every sandbox's live and test keys in plain text.
- The console skips the login for callers arriving on loopback.
- Card numbers are stored in full, because the whole point is to replay a
  specific card's behaviour.
- Nothing it serves is encrypted at rest.

None of that is a vulnerability in the intended deployment. It is why the
compose file publishes the console on `127.0.0.1` only.

## Do not put real data in it

No real card numbers, no production merchant keys, no customer data. The stand
is built to be readable, not confidential.

## What is worth reporting

- Anything that lets one sandbox read or change another sandbox's data. Sandbox
  isolation is a real boundary and a break in it is a real bug.
- Console authentication bypass from a non-loopback address, or the
  private-network trust setting doing more than it says.
- Anything that turns the stand into a way into the host it runs on — command
  injection, path traversal, SSRF through a configurable upstream.
- Dependency vulnerabilities that are actually reachable from this code.
