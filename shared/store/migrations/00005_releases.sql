-- +goose Up
-- A release is the image produced after a successful build when .opsora.yml
-- has a deploy section. At most one release per repository is active.
CREATE TABLE releases (
    id          BIGSERIAL PRIMARY KEY,
    repository  TEXT NOT NULL,
    image       TEXT NOT NULL,
    commit_sha  TEXT NOT NULL,
    build_id    BIGINT REFERENCES builds (id),
    status      TEXT NOT NULL CHECK (status IN ('active', 'failed', 'superseded', 'rolled_back')),
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX releases_one_active ON releases (repository) WHERE status = 'active';
CREATE INDEX releases_build_idx ON releases (build_id);

-- Rollback requests. The worker claims them; chat commands never touch Redis.
CREATE TABLE deploy_jobs (
    id          BIGSERIAL PRIMARY KEY,
    repository  TEXT NOT NULL,
    action      TEXT NOT NULL CHECK (action IN ('rollback')),
    status      TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    error       TEXT NOT NULL DEFAULT '',
    chat_id     TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE INDEX deploy_jobs_queued_idx ON deploy_jobs (id) WHERE status = 'queued';

-- +goose Down
DROP TABLE deploy_jobs;
DROP TABLE releases;
