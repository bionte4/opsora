# Opsora

Opsora is a self-hosted CI platform written in Go. It receives GitHub webhooks,
runs each build's steps in isolated Docker containers, reports the result back
to GitHub as a commit status, announces it on Telegram, and exposes metrics to
Prometheus and Grafana.

It is the first part of a larger "DevOps all-in-one" platform. Deployments,
ChatOps, and an AI assistant backed by pgvector are planned but not built yet.

## How it works

```
GitHub ──webhook──▶ api-server ──build id──▶ Redis (build_queue)
                        │                         │
                        ▼                         ▼
                    Postgres ◀──status, logs── worker ──▶ Docker (one container per step)
                        │                         │
                        ▼                         └──▶ GitHub commit status
                    notifier ──▶ Telegram

api-server, worker, notifier ──/metrics──▶ Prometheus ──▶ Grafana
```

- **api-server** checks the webhook signature, records the delivery (duplicate
  deliveries are ignored), stores a build in Postgres and pushes its ID to
  Redis. It also serves a small read-only API for builds and logs.
- **worker** takes build IDs from Redis, claims the build in Postgres, clones
  the commit into a fresh Docker volume, and runs the steps from
  `.opsora.yml`, each in its own container.
- **notifier** sends one Telegram message for each finished build.
- **Postgres** is the source of truth. Redis only carries build IDs, so a lost
  Redis entry never loses a build.

### Reliability

- Workers send a heartbeat every 5 seconds. If a worker dies, a reaper requeues
  its build after 30 seconds. A build that loses its worker three times is
  marked failed.
- On `SIGTERM` the worker kills the running step container and puts the build
  back in the queue without counting it as an attempt.
- Builds that were taken from Redis but never started are returned to the
  queue automatically.
- A build is marked as announced only after Telegram accepts the message, and
  the row is locked while it is being sent, so each build gets exactly one
  message even with several notifiers running.

### Build isolation

- Each step runs with `/bin/sh -c` inside the image you choose, limited to 2 GB
  of memory, 2 CPUs and 1024 processes, with `no-new-privileges`.
- Step containers never get the Docker socket or any platform secrets.
- The GitHub token is only used for the clone (passed as an HTTP header) and is
  not written to `.git/config`.
- Containers and volumes are labelled per build and removed afterwards.

## Quick start

Requirements: Docker with Compose, and a GitHub repository you can configure
webhooks for.

```sh
cp .env.example .env
# Edit .env: set GITHUB_WEBHOOK_SECRET and GRAFANA_ADMIN_PASSWORD at minimum.
docker compose up -d --build
```

| Service    | URL                     |
| ---------- | ----------------------- |
| API        | http://localhost:8080   |
| Prometheus | http://localhost:9090   |
| Grafana    | http://localhost:3000   |

Check that the API can reach its datastores:

```sh
curl localhost:8080/health
# {"status":"ok","database":"ok","redis":"ok"}
```

### Connect a GitHub repository

1. GitHub has to reach the API, so expose port 8080 through a public URL or a
   tunnel (for example `cloudflared` or `ngrok`).
2. In the repository settings, add a webhook:
   - Payload URL: `https://<your-host>/api/webhook/github`
   - Content type: `application/json`
   - Secret: the value of `GITHUB_WEBHOOK_SECRET`
   - Events: pushes and pull requests
3. Add a `.opsora.yml` to the repository (see below) and push.

To build private repositories and report commit statuses, set `GITHUB_TOKEN`
to a token with read access to contents and write access to commit statuses.
Set `OPSORA_PUBLIC_URL` to the public API URL so GitHub statuses and Telegram
messages link to the build logs.

### Telegram notifications

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy its token.
2. Send the bot a message, then read your chat ID from
   `https://api.telegram.org/bot<token>/getUpdates`.
3. Set `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` in `.env` and run
   `docker compose up -d notifier`.

`TELEGRAM_NOTIFY=failures` limits messages to failed builds. A failure message
includes the repository, ref, commit link, duration, failed step, error, and the
last lines of that step's log. Without a token the notifier stays idle.

## Pipeline file

Put `.opsora.yml` at the root of the repository being built:

```yaml
image: golang:1.27        # default image for every step
timeout: 15m              # whole build; default 30m, maximum 2h
env:
  GOCACHE: /workspace/.cache/go-build
steps:
  - name: test
    run: go test ./...
  - name: lint
    image: golangci/golangci-lint:latest   # per-step image override
    timeout: 5m                            # per-step timeout
    run: golangci-lint run
```

- The repository is checked out in `/workspace`, which is shared by all steps of
  a build. A `checkout` step is added automatically.
- Steps run in order and the build stops at the first failure; remaining steps
  are marked cancelled.
- Every step gets `CI=true`, `OPSORA_BUILD_ID`, `OPSORA_REPOSITORY`,
  `OPSORA_REF`, `OPSORA_COMMIT_SHA` and `OPSORA_STEP`.
- Unknown fields, duplicate step names and more than 50 steps are rejected,
  and the build fails with a clear error.

This repository builds itself with its own [`.opsora.yml`](.opsora.yml).

## API

| Method | Path                       | Description                                              |
| ------ | -------------------------- | -------------------------------------------------------- |
| GET    | `/health`                  | API, Postgres and Redis status                           |
| POST   | `/api/webhook/github`      | GitHub webhook receiver (`push`, `pull_request`)         |
| GET    | `/api/builds?limit=20`     | Latest builds (limit 1 to 100)                           |
| GET    | `/api/builds/:id`          | One build with its steps                                 |
| GET    | `/api/builds/:id/logs`     | Logs; `step=<position>`, `after=<id>`, `limit`, `format=text` |

To follow a running build, pass the `X-Next-After` response header back as
`after`.

## Metrics

Each service serves Prometheus metrics on its own port: api-server on 9101,
worker on 9102, notifier on 9103. Grafana starts with the provisioned
"Opsora overview" dashboard, which shows queue length, running builds, build
duration (p50 and p95), builds by status, failure rate, step duration,
webhooks, requeues, Telegram notifications and API latency.

Main metrics:

- `opsora_queue_length{queue}`
- `opsora_builds_running`
- `opsora_builds_finished_total{status}`
- `opsora_build_duration_seconds{status}`
- `opsora_step_duration_seconds{status}`
- `opsora_builds_requeued_total{reason}`
- `opsora_webhooks_total{event,result}`
- `opsora_notifications_total{result}`
- `opsora_http_request_duration_seconds{route,method,code}`

## Development

The code is a Go workspace (`go.work`) with four modules:

| Module        | Contents                                                       |
| ------------- | -------------------------------------------------------------- |
| `api-server/` | Fiber HTTP server: webhook, builds API, request metrics        |
| `worker/`     | Queue consumer, pipeline runner, Docker client, GitHub statuses |
| `notifier/`   | Telegram notifications                                         |
| `shared/`     | Postgres store and migrations, Redis queue, metrics, env loading |

Database migrations live in `shared/store/migrations` and run automatically
when any service starts.

To run a service on the host instead of in Compose, start only the
datastores and run it from its folder. Each service reads `.env` from its own
directory or the repository root:

```sh
docker compose up -d postgres redis
cd api-server && go run .
```

Before sending a change, run the same checks as the pipeline:

```sh
go vet ./api-server/... ./worker/... ./notifier/... ./shared/...
go test ./api-server/... ./worker/... ./notifier/... ./shared/...
```

Container images are built from the single root `Dockerfile` with
`--build-arg SERVICE=<api-server|worker|notifier>`.

## Security notes

- The worker container mounts `/var/run/docker.sock`, which is equivalent to
  root access on the host. Run Opsora only on a machine dedicated to CI and
  only build repositories you trust.
- Never commit `.env`; it is ignored by git and excluded from Docker builds.
- Change the default Postgres and Redis passwords before exposing the host.
