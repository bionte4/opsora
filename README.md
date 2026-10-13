# Opsora

Opsora is a self-hosted CI platform written in Go. It receives GitHub webhooks,
runs each build's steps in isolated Docker containers, reports the result back
to GitHub as a commit status, announces it on Telegram with an AI explanation
of failures, and exposes metrics to Prometheus and Grafana.

It is the first part of a larger "DevOps all-in-one" platform. Deployments,
ChatOps, search over past failures with pgvector, and change risk scores are
planned but not built yet.

## How it works

```
GitHub ──webhook──▶ api-server ──build id──▶ Redis (build_queue)
                        │                         │
                        ▼                         ▼
                    Postgres ◀──status, logs── worker ──▶ Docker (one container per step)
                        │                         │
                        ▼                         └──▶ GitHub commit status
                    notifier ──▶ Telegram
                        │
                        ▼
                    ai-service ──▶ Ollama (on the host)

api-server, worker, notifier, ai-service ──/metrics──▶ Prometheus ──▶ Grafana
```

- **api-server** checks the webhook signature, records the delivery (duplicate
  deliveries are ignored), stores a build in Postgres and pushes its ID to
  Redis. It also serves a small read-only API for builds and logs.
- **worker** takes build IDs from Redis, claims the build in Postgres, clones
  the commit into a fresh Docker volume, and runs the steps from
  `.opsora.yml`, each in its own container.
- **notifier** sends one Telegram message for each finished build. For failed
  builds it first asks ai-service for an analysis.
- **ai-service** explains failed builds with a language model and stores the
  result. Builds and notifications never depend on it: if it is down or slow,
  the message is sent without the analysis.
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

### AI analysis of failed builds

ai-service sends the last 60 lines of the failed step's log to the model and
asks for a short summary, a likely cause, and the numbers of the log lines that
show the error. Cited line numbers are checked against the log, and the
Telegram message quotes those lines so readers can verify the explanation.
Each analysis is stored, so a build is analyzed once.

It uses [Ollama](https://ollama.com) on the host, because Docker on macOS
cannot use the GPU:

```sh
ollama pull phi3
```

Set `AI_MODEL` to use another installed model and `AI_LANGUAGE` (for example
`Indonesian`) to change the language of the summary. On a MacBook Air M1 with
8 GB of RAM, phi3 takes about 25 seconds per analysis and holds about 4 GB
while loaded; `AI_KEEP_ALIVE` (default `2m`) controls how soon it is unloaded.
To turn analysis off, set `AI_SERVICE_URL=` (empty) in `.env`.

Log output is treated as untrusted data in the prompt, but a model can still be
misled by it, so messages label the analysis "may be wrong".

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
| GET    | `/api/builds/:id/analysis` | AI analysis of a failed build, with the cited log lines  |
| GET    | `/api/integrations/ai`     | AI integration status: reachable, model installed, latency |
| POST   | `/api/integrations/ai/test` | Test connection: runs a tiny generation (can take ~10s) |

To follow a running build, pass the `X-Next-After` response header back as
`after`.

The integration endpoints always answer `200` with `configured`, plus
`connected`/`model_available` (status) or `ok` (test) and an `error` message,
so a dashboard card can render every outcome from one response shape.

## Metrics

Each service serves Prometheus metrics on its own port: api-server on 9101,
worker on 9102, notifier on 9103, ai-service on 9104. Grafana starts with the provisioned
"Opsora overview" dashboard, which shows queue length, running builds, build
duration (p50 and p95), builds by status, failure rate, step duration,
webhooks, requeues, Telegram notifications, API latency, and AI requests
and durations.

Main metrics:

- `opsora_queue_length{queue}`
- `opsora_builds_running`
- `opsora_builds_finished_total{status}`
- `opsora_build_duration_seconds{status}`
- `opsora_step_duration_seconds{status}`
- `opsora_builds_requeued_total{reason}`
- `opsora_webhooks_total{event,result}`
- `opsora_notifications_total{result}`
- `opsora_ai_requests_total{kind,result}`
- `opsora_ai_duration_seconds{kind}`
- `opsora_http_request_duration_seconds{route,method,code}`

## Development

The code is a Go workspace (`go.work`) with five modules:

| Module        | Contents                                                       |
| ------------- | -------------------------------------------------------------- |
| `api-server/` | Fiber HTTP server: webhook, builds API, request metrics        |
| `worker/`     | Queue consumer, pipeline runner, Docker client, GitHub statuses |
| `notifier/`   | Telegram notifications                                         |
| `ai-service/` | Failure analysis with a language model (Ollama)                |
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
go vet ./api-server/... ./worker/... ./notifier/... ./ai-service/... ./shared/...
go test ./api-server/... ./worker/... ./notifier/... ./ai-service/... ./shared/...
```

Container images are built from the single root `Dockerfile` with
`--build-arg SERVICE=<api-server|worker|notifier|ai-service>`.

## Security notes

- The worker container mounts `/var/run/docker.sock`, which is equivalent to
  root access on the host. Run Opsora only on a machine dedicated to CI and
  only build repositories you trust.
- Never commit `.env`; it is ignored by git and excluded from Docker builds.
- Change the default Postgres and Redis passwords before exposing the host.
