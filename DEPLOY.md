# Deploying payme-mock

This document covers running the stand on a server rather than on a laptop.
There are two ways, and they are separate tracks — pick one and read only it:

- **[Track A — Docker Compose](#track-a--docker-compose).** Everything the
  stand needs comes from this repository and nothing is installed on the host
  beyond Docker. Use it unless something stops you.
- **[Track B — no Docker](#track-b--no-docker).** Three systemd units, a
  Postgres the host already has, and binaries you build once. Use it where
  Docker is not available or not allowed, or where Postgres is managed
  elsewhere.

Both end in the same place: three services, one database, a proxy in front. The
sections after the two tracks — proxy, backups, logs — apply to either, and say
so where the commands differ.

Read [README.md](README.md) first for what the services are and how they talk to
each other. This file assumes that context and only covers the parts that change
when the stand stops being local.

> **This is a testing tool holding test credentials, not a payment processor.**
> It is not built to face the open internet. Deploy it where your team can
> reach it and nobody else can — a private network, a VPN, or behind an
> authenticating proxy. The console alone exposes every sandbox's keys.

---

## What the stand is made of

Three services run today, whichever track you take:

| Service     | Port             | Faces                                        |
|-------------|------------------|----------------------------------------------|
| `paymemock` | `8082`           | Your backend, which calls the Subscribe API. |
| `merchant`  | `8081`           | The example merchant endpoint.               |
| `console`   | `127.0.0.1:8080` | Operators, through the proxy only.           |

Plus PostgreSQL. `worker` and `gateway` are configured but not yet written, so
until `gateway` exists, TLS and the public hostname are the proxy's job rather
than the stand's.

Two things about that table are worth knowing before either track:

- **The ports move**, from `.env` in Track A and from the unit files in Track B.
  Nothing in the Go code hardcodes one; every service reads `HTTP_ADDR`.
- **`console` is on loopback deliberately.** It shows every sandbox's keys and
  it is the one binding in this document that can expose them.

**Redis is not needed.** `REDIS_ADDR` exists in the configuration and no code
dials it — the queue belongs to `worker`, which has no `main` yet. Track A
starts a Redis container because the commented-out `worker` block will need one;
Track B does not install Redis at all.

---

# Track A — Docker Compose

## What you need

- A Linux host with Docker Engine 24+ and the Compose plugin.
- 2 GB of RAM and 10 GB of disk to start. Postgres holds the traffic log, which
  is the part that grows.
- A DNS name and a TLS certificate if the stand is reachable from anywhere but
  the host itself.

Nothing else. Go is only needed to build outside Docker; the images build the
binaries themselves from `Dockerfile`.

## First deployment

```sh
git clone https://github.com/bakhod1r/payme-mock.git
cd payme-mock
cp .env.example .env
```

Then edit `.env`. Four values decide whether the deployment is safe and whether
the two sides can find each other:

```sh
# Required. The console refuses to start without it, by design.
CONSOLE_USER=admin
CONSOLE_PASSWORD=<a long random string>

# The base of each sandbox's endpoint URL — what gets pasted into cash
# register settings. Must be the address callers actually reach, not localhost.
GATEWAY_BASE_URL=https://payme-mock.example.uz

# Point the merchant at the emulator, or at the real provider to run against
# the real thing. No merchant code changes either way.
SUBSCRIBE_BASE_URL=http://paymemock:8082/api
```

Two more are localhost by default and wrong the moment the stand is not local.
Neither breaks a boot, so a deployment that leaves them tends to look healthy
until someone clicks something:

```sh
# The base of generated checkout links, which a payer's browser opens. Left at
# localhost, every link points at the payer's own machine.
CHECKOUT_BASE_URL=https://payme-mock.example.uz

# Where the stand's own APIs answer. It is what the console writes into the
# copyable curl on a logged call, so it has to reach the stand from the
# operator's terminal, not from inside the container.
STAND_BASE_URL=https://payme-mock.example.uz
```

Generate the password rather than choosing one:

```sh
openssl rand -base64 32
```

`.env` now holds a credential. Keep it out of the repository — it is already
ignored — and off shared drives. On a real host, `chmod 600 .env`.

Bring the stand up:

```sh
docker compose up -d --build
```

The first build compiles the binaries and takes a few minutes. Later builds
reuse the module and build caches mounted in the `Dockerfile` and are much
faster.

## Moving the published ports

Every host-side port comes from `.env`:

```sh
POSTGRES_HOST_PORT=5433
REDIS_HOST_PORT=6380
MERCHANT_HOST_PORT=8081
PAYMEMOCK_HOST_PORT=8082
CONSOLE_HOST_PORT=8080
CONSOLE_BIND_ADDR=127.0.0.1
```

Only the host side moves. Inside the Compose network each container keeps
listening on the fixed port its siblings address it by, which is why
`MERCHANT_BASE_URL` stays `http://merchant:8081` no matter what the host
publishes. Two consequences:

- Moving `POSTGRES_HOST_PORT` means editing the `5433` in `DATABASE_URL` too.
  The containers reach `postgres:5432` directly and do not care.
- `CONSOLE_BIND_ADDR` is `127.0.0.1` for the reason under
  [Putting a proxy in front](#putting-a-proxy-in-front). Widening it is the one
  change here that can expose every sandbox's keys.

## Verifying

```sh
curl -fsS http://localhost:8081/healthz   # merchant
curl -fsS http://localhost:8082/healthz   # paymemock
curl -fsS http://localhost:8080/healthz   # console
```

Then open the console at `http://127.0.0.1:8080` — through an SSH tunnel if you
are not on the host — and confirm the login prompt appears and your password
works. If it does not prompt at all, see
[the trusted-network setting](#the-trusted-network-setting).

## Updating

```sh
git pull
docker compose up -d --build
```

Compose recreates only the containers whose image changed. Postgres keeps its
volume and stays up. Read [CHANGELOG.md](CHANGELOG.md) before an update that
crosses a release — a migration that drops a column is worth a `pg_dump` first.

---

# Track B — no Docker

Three static binaries and a systemd unit each. The stand is unusually easy to
deploy this way, for one reason: the schema migrations and the console's HTML
templates are compiled into the binaries with `go:embed`, and the build sets
`CGO_ENABLED=0`. Each service is a single file that depends on nothing on the
host but a C library it does not use and a Postgres it connects to over TCP.

## What you need

- A Linux host with systemd.
- **PostgreSQL 14 or newer**, on this host or reachable from it. The Compose
  track pins 18; nothing in the schema needs it.
- **Go 1.26.2 or newer**, to build. Only on the build machine — the binaries
  carry no runtime dependency on the toolchain, so building elsewhere and
  copying three files over is a supported deployment.
- No Redis, no Node, no `ca-certificates` beyond what the distribution has.

## Building

```sh
git clone https://github.com/bakhod1r/payme-mock.git
cd payme-mock

mkdir -p bin
for svc in merchant paymemock console; do
    CGO_ENABLED=0 go build -trimpath -o bin/$svc ./cmd/$svc
done
```

Cross-compiling from a laptop to a Linux server is the same command with two
variables:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/merchant ./cmd/merchant
```

Install them where systemd can find them:

```sh
sudo install -m 0755 bin/merchant bin/paymemock bin/console /usr/local/bin/
```

## The database

Create a role and a database. The names match the rest of the documentation, so
the connection strings below can be pasted as they are:

```sh
sudo -u postgres psql <<'SQL'
CREATE ROLE payme LOGIN PASSWORD 'change-me';
CREATE DATABASE paymemock OWNER payme;
SQL
```

Nothing else. `DB_MIGRATE_ON_START=true` is the default and the services bring
the schema up themselves on first boot, out of the migrations embedded in the
binary. There is no separate migration tool to install and no `migrations/`
directory to copy to the host.

If you would rather migrate deliberately, set `DB_MIGRATE_ON_START=false` on
`merchant` and `paymemock` and apply `migrations/00001_schema.sql` with `psql`
before starting them. One caveat: **`console` migrates unconditionally** and
does not read that flag, so start it after the schema is where you want it.

## Configuration

Both tracks read the same keys through the same loader. Off Docker there are no
container names to resolve, so every address is a real one — and `HTTP_ADDR`
differs per service, which is what makes a single shared `.env` the wrong shape
here. One file per service instead:

```sh
sudo mkdir -p /etc/payme-mock
```

`/etc/payme-mock/common.env` — what all three agree on:

```sh
DATABASE_URL=postgres://payme:change-me@127.0.0.1:5432/paymemock?sslmode=disable
DB_MIGRATE_ON_START=true
LOG_LEVEL=info

# The services listen on loopback and the proxy is the only thing in front of
# them, so the peer address is the truth and there is no proxy header to trust.
# Left on, a caller reaching a service directly could name any address it likes
# and walk past the sandbox address rules.
TRUST_FORWARDED_FOR=false
```

`/etc/payme-mock/merchant.env`:

```sh
HTTP_ADDR=127.0.0.1:8081
SUBSCRIBE_BASE_URL=http://127.0.0.1:8082/api
```

`/etc/payme-mock/paymemock.env`:

```sh
HTTP_ADDR=127.0.0.1:8082
MERCHANT_BASE_URL=http://127.0.0.1:8081
CHECKOUT_BASE_URL=https://payme-mock.example.uz
```

`/etc/payme-mock/console.env`:

```sh
HTTP_ADDR=127.0.0.1:8080
CONSOLE_USER=admin
CONSOLE_PASSWORD=<openssl rand -base64 32>
GATEWAY_BASE_URL=https://payme-mock.example.uz
STAND_BASE_URL=https://payme-mock.example.uz

# The peer is the proxy on loopback, which the loopback exemption would treat as
# this machine and let past the login. Off, so the password is unconditional.
CONSOLE_OPEN_ON_LOOPBACK=false
CONSOLE_TRUST_PRIVATE_NET=false
```

Two of those files hold credentials:

```sh
sudo chown -R root:root /etc/payme-mock
sudo chmod 600 /etc/payme-mock/*.env
```

Binding to `127.0.0.1` rather than `:8081` is the difference that matters in
this track. Under Compose the network isolates the services and only published
ports are reachable; on a bare host, `HTTP_ADDR=:8081` is every interface, and
`paymemock` on a public interface is a stand anyone can drive. Let the proxy be
the only thing that reaches them.

## The units

A dedicated user, owning nothing:

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin paymemock
```

`/etc/systemd/system/payme-merchant.service`:

```ini
[Unit]
Description=payme-mock merchant
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
User=paymemock
EnvironmentFile=/etc/payme-mock/common.env
EnvironmentFile=/etc/payme-mock/merchant.env
ExecStart=/usr/local/bin/merchant
Restart=on-failure
RestartSec=2s

# The service writes nothing to disk: the schema is in the binary and the state
# is in Postgres. So it gets a filesystem it cannot touch.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
```

`payme-paymemock.service` and `payme-console.service` are the same file with the
`Description`, the second `EnvironmentFile`, and the `ExecStart` changed. The
services do not depend on each other at boot: each retries its outbound calls,
so start order does not matter.

`SHUTDOWN_TIMEOUT` defaults to `15s` — how long a service lets in-flight
requests finish. Keep systemd's own patience above it or a restart under load
turns into a kill:

```ini
TimeoutStopSec=30s
```

Bring them up:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now payme-merchant payme-paymemock payme-console
```

## Verifying

```sh
curl -fsS http://127.0.0.1:8081/healthz   # merchant
curl -fsS http://127.0.0.1:8082/healthz   # paymemock
curl -fsS http://127.0.0.1:8080/healthz   # console
systemctl status payme-console
```

The console asks for the password here even on the host itself, because both
exemptions are off in `console.env`. That is the intended state on a server.

## Updating

```sh
git pull
for svc in merchant paymemock console; do
    CGO_ENABLED=0 go build -trimpath -o bin/$svc ./cmd/$svc
done
sudo install -m 0755 bin/merchant bin/paymemock bin/console /usr/local/bin/
sudo systemctl restart payme-merchant payme-paymemock payme-console
```

Replacing a running binary in place is safe — the kernel keeps the old inode
until the process exits — so the install and the restart do not have to be
close together. Read [CHANGELOG.md](CHANGELOG.md) before an update that crosses
a release, and take a `pg_dump` first if it carries a migration.

---

# Both tracks

## Putting a proxy in front

The console is on loopback deliberately. It shows every sandbox's keys, so
exposing it on all interfaces would hand those keys to whoever can reach the
port. Leave that binding alone and let the proxy reach it over loopback.

An nginx server block that terminates TLS and forwards to the console:

```nginx
server {
    listen 443 ssl;
    server_name payme-mock.example.uz;

    ssl_certificate     /etc/letsencrypt/live/payme-mock.example.uz/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/payme-mock.example.uz/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

`paymemock` on `8082` needs to be reachable by whatever backend you are testing,
and by nothing else. If that backend is on the same host, leave the port on
loopback. If it is elsewhere, put it behind the proxy on its own hostname and
restrict it at the firewall.

### The trusted-network setting

`CONSOLE_TRUST_PRIVATE_NET=true` is set in `docker-compose.yml` for the local
case: Docker rewrites the peer address to the bridge gateway, so a browser on
the same machine arrives from `172.x` and a plain loopback test never matches.
It is safe there because the port is published to `127.0.0.1` only.

**On a real host behind a proxy, set it to `false`.** With it on and the port
reachable from a network, anything on that network is treated as local and skips
the login. Track B's `console.env` already sets it that way; a Track A
deployment has to override it in `.env`.

`CONSOLE_OPEN_ON_LOOPBACK=true` is the narrower half of the same idea. It only
ever matches a request whose peer is loopback — but on a host where the proxy
reaches the console over loopback, that is every proxied request. Set it to
`false` on a server if the login is meant to be unconditional.

### The forwarded-address setting

`TRUST_FORWARDED_FOR` decides whether `merchant` and `paymemock` read the client
address from `X-Forwarded-For` or from the peer. It defaults to `true`, which is
right under Compose, where the peer is Docker's bridge and the header is the
only real address available.

It is only ever safe when every request arrives through a proxy you control and
that proxy overwrites the header. The nginx block above sets
`X-Forwarded-For $proxy_add_x_forwarded_for`, which *appends* to whatever the
caller sent — fine for the console, but it means a request reaching `paymemock`
directly on `8082`, bypassing the proxy, can name any address it likes and walk
past the sandbox address rules. Either keep `8082` unreachable except through
the proxy, or set `TRUST_FORWARDED_FOR=false` and let the peer be the truth.
Track B does the latter, because there the services bind to loopback and the
peer already is the proxy.

## Schema migrations

`DB_MIGRATE_ON_START=true` is the default and is what makes a first boot work
with no extra step: `merchant` and `paymemock` bring the schema up before they
serve, out of the migrations embedded in the binary.

Two services racing to migrate the same database is fine for a single-host stand
and not something to rely on beyond it. If you run more than one replica of
either service, set `DB_MIGRATE_ON_START=false` and apply
`migrations/00001_schema.sql` as a separate step before the rollout, so exactly
one process touches the schema. Note that `console` migrates unconditionally and
ignores the flag, so it is not a way to freeze the schema entirely — sequence
the console's start instead.

## Backups

Everything worth keeping is in Postgres — sandboxes, keys, payments, and the
traffic log. In Track A that is the `pgdata` volume.

```sh
# Track A
docker compose exec -T postgres \
    pg_dump -U payme -d paymemock --format=custom > paymemock-$(date +%F).dump

# Track B
pg_dump -U payme -d paymemock --format=custom > paymemock-$(date +%F).dump
```

Restoring into a fresh stand:

```sh
# Track A
docker compose exec -T postgres \
    pg_restore -U payme -d paymemock --clean --if-exists < paymemock-2026-01-01.dump

# Track B
pg_restore -U payme -d paymemock --clean --if-exists < paymemock-2026-01-01.dump
```

## Logs

```sh
# Track A
docker compose logs -f            # everything
docker compose logs -f paymemock  # one service

# Track B
journalctl -u payme-paymemock -f
journalctl -u 'payme-*' -f
```

`LOG_LEVEL=info` is the default; `debug` adds per-request detail and is loud
enough that it is a debugging setting, not a deployment one.

## Two remotes

This repository is pushed to both GitHub and the internal GitLab:

```
origin  https://github.com/bakhod1r/payme-mock.git
wayii   https://gitlab.app-wayll.uz/wayll/payme-mock.git
```

Push a branch to both so the two stay in step:

```sh
git push origin  <branch>
git push wayii   <branch>
```

CI in `.github/workflows/ci.yml` runs on the GitHub side and enforces the 100%
coverage gate described in the `Makefile`.

## Troubleshooting

**The console exits immediately.** `CONSOLE_PASSWORD` is empty. In Track A,
Compose fails the variable substitution rather than starting a console with no
password; in Track B the binary reports the field by name and exits, and
`journalctl -u payme-console` has the line.

**`docker compose up` fails on a variable.** Only `CONSOLE_PASSWORD` is declared
required in the Compose file, so that is the one. Every other key has a default
and falls back silently.

**A service exits with `address already in use`.** Something on the host holds
the port. Track A: move the host side with the matching `*_HOST_PORT` in `.env`
and leave every inter-service URL alone. Track B: change `HTTP_ADDR` in that
service's env file, and the URL naming it in the other two.

**Postgres will not start after an upgrade (Track A).** The Postgres 18 image
keeps data in a major-version subdirectory, which is why the volume mounts
`/var/lib/postgresql` and not `/var/lib/postgresql/data`. A mount one level deep
leaves the data unused and the container refuses to come up.

**The merchant never receives a webhook.** The two sides are not naming each
other correctly. In Track A, `MERCHANT_BASE_URL` has to be the service name,
`http://merchant:8081` — `localhost` there means the container itself. In Track
B it is the reverse: there are no service names, so it has to be
`http://127.0.0.1:8081`. Same for `SUBSCRIBE_BASE_URL` in the other direction.

**An endpoint URL from the console does not resolve.** `GATEWAY_BASE_URL` is
still the local default. Set it to the address callers actually use and restart
the console.

**A payer's checkout link opens nothing.** `CHECKOUT_BASE_URL` is still
`localhost:8082`, so the link points at the payer's own machine.

**Every console request asks for the password on the host itself.** Expected
once the two exemptions are off. That is the correct setting on a server; use
the password.

**A Track B service cannot reach Postgres.** `DATABASE_URL` says `127.0.0.1`
while Postgres listens on a Unix socket only, which is a common distribution
default. Either enable `listen_addresses = 'localhost'` in `postgresql.conf` and
add a `host` line to `pg_hba.conf`, or point the URL at the socket directory:
`postgres:///paymemock?host=/var/run/postgresql&user=payme`.

## The rest of the settings

`.env.example` lists every key the code reads with its default and the reason it
exists — 27 of them, including the behaviour knobs a deployment rarely touches
(`IDEMPOTENCY_WINDOW`, `SHUTDOWN_TIMEOUT`, `REDIS_PASSWORD`, `REDIS_DB`) and the
ones belonging to `worker` and `gateway`, which are configured but have no
`main` yet and so are read by nothing today. It is written for Track A's single
file, but the keys are the same ones Track B splits across
`/etc/payme-mock/*.env`. Treat that file as the inventory; this document only
covers what changes when the stand stops being local.
