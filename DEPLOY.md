# Deploying payme-mock

This document covers running the stand on a server rather than on a laptop —
one host, Docker Compose, a reverse proxy in front. Everything the stand needs
is in this repository; there is nothing to install on the host beyond Docker.

Read [README.md](README.md) first for what the services are and how they talk
to each other. This file assumes that context and only covers the parts that
change when the stand stops being local.

> **This is a testing tool holding test credentials, not a payment processor.**
> It is not built to face the open internet. Deploy it where your team can
> reach it and nobody else can — a private network, a VPN, or behind an
> authenticating proxy. The console alone exposes every sandbox's keys.

---

## What you need

- A Linux host with Docker Engine 24+ and the Compose plugin.
- 2 GB of RAM and 10 GB of disk to start. Postgres holds the traffic log,
  which is the part that grows.
- A DNS name and a TLS certificate if the stand is reachable from anywhere but
  the host itself.

Nothing else. Go is only needed to build outside Docker; the images build the
binaries themselves from `Dockerfile`.

## The shape of a deployment

Five containers come up from `docker-compose.yml`:

| Container   | Port                 | Faces |
|-------------|----------------------|-------|
| `postgres`  | `5433` on the host   | Nothing outside the host. |
| `redis`     | `6380` on the host   | Nothing outside the host. |
| `paymemock` | `8082`               | Your backend, which calls the Subscribe API. |
| `merchant`  | `8081`               | The example merchant endpoint. |
| `console`   | `127.0.0.1:8080`     | Operators, through the proxy only. |

`worker` and `gateway` are configured but not yet written, so their blocks stay
commented out. Until `gateway` exists, TLS and the public hostname are the
proxy's job, not the stand's.

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

The first build compiles four Go binaries and takes a few minutes. Later builds
reuse the module and build caches mounted in the `Dockerfile` and are much
faster.

## Verifying

Every service answers `GET /healthz`:

```sh
curl -fsS http://localhost:8081/healthz   # merchant
curl -fsS http://localhost:8082/healthz   # paymemock
curl -fsS http://localhost:8080/healthz   # console
```

Then open the console at `http://127.0.0.1:8080` — through an SSH tunnel if you
are not on the host — and confirm the login prompt appears and your password
works. If it does not prompt at all, see the trusted-network note below.

## Schema migrations

`DB_MIGRATE_ON_START=true` is the default, and it is what makes the first boot
work with no extra step: `merchant` and `paymemock` bring the schema up before
they serve.

Two services racing to migrate the same database is fine for a single-host
stand and not something to rely on beyond it. If you deploy more than one
replica of either service, set `DB_MIGRATE_ON_START=false` and run the
migrations in `migrations/` as a separate step before the rollout, so exactly
one process touches the schema.

## Putting a proxy in front

The console is published on `127.0.0.1:8080` deliberately. It shows every
sandbox's keys, so publishing it on all interfaces would hand those keys to
whoever can reach the port. Leave that binding alone and let the proxy reach it
over loopback.

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

`paymemock` on `8082` needs to be reachable by whatever backend you are
testing, and by nothing else. If that backend is on the same host, leave the
port on loopback too. If it is elsewhere, put it behind the proxy on its own
hostname and restrict it at the firewall.

### The trusted-network setting

`CONSOLE_TRUST_PRIVATE_NET=true` in `docker-compose.yml` exists for the local
case: Docker rewrites the peer address to the bridge gateway, so a browser on
the same machine arrives from `172.x` and a plain loopback test never matches.
It is safe there because the port is published to `127.0.0.1` only.

**Behind a proxy on a real host, set it to `false`.** With it on and the port
reachable from a network, anything on that network is treated as local and
skips the login.

## Backups

Everything worth keeping is in the `pgdata` volume — sandboxes, keys, payments,
and the traffic log.

```sh
docker compose exec -T postgres \
    pg_dump -U payme -d paymemock --format=custom > paymemock-$(date +%F).dump
```

Restoring into a fresh stand:

```sh
docker compose exec -T postgres \
    pg_restore -U payme -d paymemock --clean --if-exists < paymemock-2026-01-01.dump
```

Redis holds nothing that has to survive a restart, so it needs no backup.

## Updating

```sh
git pull
docker compose up -d --build
```

Compose recreates only the containers whose image changed. Postgres and Redis
keep their volumes and stay up. Read [CHANGELOG.md](CHANGELOG.md) before an
update that crosses a release — a migration that drops a column is worth a
`pg_dump` first.

## Logs

```sh
docker compose logs -f            # everything
docker compose logs -f paymemock  # one service
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

**The console exits immediately.** `CONSOLE_PASSWORD` is empty. Compose fails
the variable substitution rather than starting a console with no password.

**Postgres will not start after an upgrade.** The Postgres 18 image keeps data
in a major-version subdirectory, which is why the volume mounts
`/var/lib/postgresql` and not `/var/lib/postgresql/data`. A mount one level
deep leaves the data unused and the container refuses to come up.

**The merchant never receives a webhook.** `MERCHANT_BASE_URL` inside a
container has to be the service name, `http://merchant:8081` — `localhost`
there means the container itself. Same for `SUBSCRIBE_BASE_URL` in the other
direction.

**An endpoint URL from the console does not resolve.** `GATEWAY_BASE_URL` is
still the local default. Set it to the address callers actually use and restart
the console.
