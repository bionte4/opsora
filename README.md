# Opsora

Opsora is a self-hosted CI platform written in Go. It receives GitHub webhooks,
runs each build's steps in isolated Docker containers, reports the result back
to GitHub as a commit status, announces it on Telegram with an AI explanation
of failures, and exposes metrics to Prometheus and Grafana.

It is the first part of a larger "DevOps all-in-one" platform. Similar past
failures are searchable with pgvector when an embedding model is configured.
Calibrated change-risk scores are not built yet.

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
  Redis. It also serves the landing page, the signed-in console, and the
  builds API.
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
webhooks for. Minimum hardware and the VPS steps are in
[Deploy on a VPS](docs/deploy-vps.md).

```sh
cp .env.example .env
# Edit .env: set GITHUB_WEBHOOK_SECRET and GRAFANA_ADMIN_PASSWORD at minimum.
docker compose up -d --build
```

| Service    | URL                     |
| ---------- | ----------------------- |
| Console    | http://localhost:8080   |
| Prometheus | http://localhost:9090   |
| Grafana    | http://localhost:3000   |

Open the console and sign in. The first API start creates one admin when no
users exist yet:

| Field    | Default              |
| -------- | -------------------- |
| Email    | `admin@opsora.local` |
| Password | `change-me`          |

Set `ADMIN_EMAIL` and `ADMIN_PASSWORD` in `.env` before that first start if you
do not want the defaults. Later starts do not reset the account. Change the
password from **Pengguna** in the console, or set a new `ADMIN_PASSWORD` only
on a database that still has no users.

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

By default it uses [Ollama](https://ollama.com) on the host, because Docker on
macOS cannot use the GPU:

```sh
ollama pull phi3
```

Set `AI_MODEL` to use another installed model and `AI_LANGUAGE` (for example
`Indonesian`) to change the language of the summary. On a MacBook Air M1 with
8 GB of RAM, phi3 takes about 25 seconds per analysis and holds about 4 GB
while loaded; `AI_KEEP_ALIVE` (default `2m`) controls how soon it is unloaded.
To turn analysis off, set `AI_SERVICE_URL=` (empty) in `.env`.

To use a hosted model instead, set `AI_PROVIDER=openai`, `AI_BASE_URL` to an
OpenAI-compatible endpoint, and `AI_API_KEY`. Groq's free tier is
`https://api.groq.com/openai/v1` with a model such as `llama-3.1-8b-instant`.
The same integration card and connection test then talk to that provider. The
key is not logged. The request includes about 60 lines of the failed step's
log.

Set `AI_EMBED_MODEL` to store a vector of each explanation and show similar
past failures on the build page. For Ollama that model is `nomic-embed-text`
(`ollama pull nomic-embed-text`). The vector is 768 dimensions. A match of
75% or more is listed, and the page calls that a repeated-failure risk. It is
similarity to explanations already stored, not a prediction about the diff.
Hosted chat providers that have no embeddings API leave this blank.

Log output is treated as untrusted data in the prompt, but a model can still be
misled by it, so messages label the analysis "may be wrong".

### Chat commands

The notifier also polls the bot for commands and calls the API. The API holds
the allowlist, so a chat that is not listed is rejected:

| Command | Action |
| --- | --- |
| `/status` | Latest builds. `/status 17` shows one build. |
| `/logs 17` | Last log lines of a build. |
| `/rerun 17` | Queues a new build of that commit. |
| `/rollback` | Restores the active release. Add the repository name when more than one has been deployed. |

Set `CHATOPS_TOKEN` to a long random string (the same value in the API and the
notifier) and `CHATOPS_ALLOWED_CHATS` to a comma-separated list of chat IDs.
Without the token, commands are disabled and notifications still work.

### Deploy and rollback

A repository can opt in to a deploy after the pipeline succeeds:

```yaml
deploy:
  image: ghcr.io/acme/app   # lowercase, no tag; Opsora adds the commit SHA
  dockerfile: Dockerfile    # optional, default Dockerfile
  settle: 5s                # how long the new container must stay up
```

The worker builds that image from the commit that just passed, tags it
`<image>:<commit sha>` and `<image>:current`, and starts a container named
`opsora-deploy-<repository>`. The build stays green if the deploy fails; the
release is recorded as failed and the previous active release is kept. The
failure message includes `/rollback`.

`/rollback` starts the active release's image again and marks the failed
release rolled back. `image:current` always points at the active release.

The deploy container has no mounts and no platform secrets. It is limited to
1 GB of memory and 1 CPU. `docker build` itself runs on the host's Docker
daemon, so only enable `deploy` for repositories you trust.

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

## Web console

Who can sign in, and what each role does next, is in [Console flows](docs/alur-konsol.md).

`GET /` is the public landing page. The signed-in console is under `/app`.
Pages are server-rendered and the copy is Indonesian. A session cookie lasts
14 days. Forms send a CSRF token that must match the `opsora_csrf` cookie.

| Page            | Path                 | Who can open it                          |
| --------------- | -------------------- | ---------------------------------------- |
| Dashboard       | `/app`               | anyone signed in; build numbers need `builds.read` |
| Build list      | `/app/builds`        | `builds.read`                            |
| Build detail    | `/app/builds/:id`    | `builds.read`; rerun needs `builds.rerun` |
| Users           | `/app/users`         | `users.read`; create and edit need `users.write` |
| Roles           | `/app/roles`         | `roles.read`; create and edit need `roles.write` |
| Integrations    | `/app/integrations`  | `integrations.read`; the test button needs `integrations.test` |

The landing page shows whether the AI service is reachable. Generating a test
reply is only available after sign-in, so an anonymous visit does not load the
model.

Each user has one role. The built-in roles are rewritten from code on every
start:

| Role       | Permissions                                                                 |
| ---------- | --------------------------------------------------------------------------- |
| `admin`    | everything below                                                            |
| `operator` | `builds.read`, `builds.rerun`, `integrations.read`, `integrations.test`    |
| `viewer`   | `builds.read`, `integrations.read`                                          |

Custom roles are created in the console. Names are lowercase letters, digits,
and hyphens, and cannot be `admin`, `operator`, or `viewer`. System roles
cannot be edited or deleted. A role that still has users cannot be deleted.
The last active admin cannot be demoted, disabled, or deleted, and you cannot
delete the account you are signed in with.

The password is never logged. A warning is printed when `ADMIN_PASSWORD` is
still `change-me`. Health checks stay at `GET /health` and still return JSON.

## API

| Method | Path                       | Description                                              |
| ------ | -------------------------- | -------------------------------------------------------- |
| GET    | `/`                        | Public landing page                                      |
| GET    | `/health`                  | API, Postgres and Redis status                           |
| POST   | `/api/webhook/github`      | GitHub webhook receiver (`push`, `pull_request`)         |
| GET    | `/api/builds?limit=20`     | Latest builds. Session with `builds.read`, or `Authorization: Bearer $API_READ_TOKEN` |
| GET    | `/api/builds/:id`          | One build with its steps. Same authentication            |
| GET    | `/api/builds/:id/logs`     | Logs; `step=<position>`, `after=<id>`, `limit`, `format=text`. Same authentication |
| GET    | `/api/builds/:id/analysis` | AI analysis of a failed build, with the cited log lines  |
| GET    | `/api/builds/:id/similar`  | Up to three earlier failures with a similar explanation  |
| GET    | `/api/integrations/ai`     | AI integration status: reachable, model installed, latency |
| POST   | `/api/integrations/ai/test` | Test connection: runs a tiny generation (can take ~10s) |
| GET    | `/api/chatops/builds`      | Latest builds. Requires `Authorization: Bearer $CHATOPS_TOKEN` and header `X-Opsora-Chat-Id` |
| POST   | `/api/chatops/builds/:id/rerun` | Queue a new build of that commit |
| POST   | `/api/chatops/rollback`    | Restore the active release. Body `{"repository":"org/repo"}` |
| GET    | `/api/chatops/jobs/:id`    | Rollback job status |

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
- `opsora_chatops_commands_total{command,result}`
- `opsora_deploys_total{status}`
- `opsora_http_request_duration_seconds{route,method,code}`

## Development

The code is a Go workspace (`go.work`) with five modules:

| Module        | Contents                                                       |
| ------------- | -------------------------------------------------------------- |
| `api-server/` | Fiber HTTP server: landing page, console, webhook, builds API |
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
  only build repositories you trust. A `deploy` section runs `docker build` on
  that daemon, so a Dockerfile can do more than a pipeline step can.
- Chat commands are rejected unless the chat ID is in `CHATOPS_ALLOWED_CHATS`
  and the caller presents `CHATOPS_TOKEN`. `GET /api/builds` requires a
  signed-in user with `builds.read`, or `Authorization: Bearer $API_READ_TOKEN`
  when that variable is set. GitHub and Telegram links point at `/app/builds/:id`,
  which asks the reader to sign in. The landing page and `GET /health` stay
  public. Postgres, Redis, Prometheus and Grafana listen on `127.0.0.1` only.
- Change `ADMIN_PASSWORD` before anyone else can reach port 8080. The default
  admin is only created when the users table is empty.
- Never commit `.env`; it is ignored by git and excluded from Docker builds.
- Change the default Postgres and Redis passwords before exposing the host.
