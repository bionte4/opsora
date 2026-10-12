-- +goose Up
CREATE TABLE deliveries (
    id          TEXT PRIMARY KEY,
    event       TEXT NOT NULL,
    payload     JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE builds (
    id           BIGSERIAL PRIMARY KEY,
    delivery_id  TEXT NOT NULL UNIQUE REFERENCES deliveries (id),
    repository   TEXT NOT NULL,
    ref          TEXT NOT NULL DEFAULT '',
    commit_sha   TEXT NOT NULL DEFAULT '',
    actor        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'queued'
                 CHECK (status IN ('queued', 'running', 'success', 'failed')),
    error        TEXT NOT NULL DEFAULT '',
    attempts     INT NOT NULL DEFAULT 0,
    worker_id    TEXT,
    heartbeat_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ
);

CREATE INDEX builds_created_at_idx ON builds (created_at DESC);
CREATE INDEX builds_running_heartbeat_idx ON builds (heartbeat_at) WHERE status = 'running';

CREATE TABLE build_steps (
    build_id    BIGINT NOT NULL REFERENCES builds (id) ON DELETE CASCADE,
    position    INT NOT NULL,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'running', 'success', 'failed', 'cancelled')),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    PRIMARY KEY (build_id, position)
);

-- +goose Down
DROP TABLE build_steps;
DROP TABLE builds;
DROP TABLE deliveries;
