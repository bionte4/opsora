-- +goose Up
ALTER TABLE build_steps DROP CONSTRAINT build_steps_status_check;
ALTER TABLE build_steps ADD CONSTRAINT build_steps_status_check
    CHECK (status IN ('pending', 'running', 'success', 'failed', 'timed_out', 'cancelled'));

CREATE TABLE build_logs (
    id         BIGSERIAL PRIMARY KEY,
    build_id   BIGINT NOT NULL,
    position   INT NOT NULL,
    stream     TEXT NOT NULL CHECK (stream IN ('stdout', 'stderr', 'system')),
    line       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (build_id, position) REFERENCES build_steps (build_id, position) ON DELETE CASCADE
);

CREATE INDEX build_logs_build_idx ON build_logs (build_id, id);

-- +goose Down
DROP TABLE build_logs;
UPDATE build_steps SET status = 'failed' WHERE status = 'timed_out';
ALTER TABLE build_steps DROP CONSTRAINT build_steps_status_check;
ALTER TABLE build_steps ADD CONSTRAINT build_steps_status_check
    CHECK (status IN ('pending', 'running', 'success', 'failed', 'cancelled'));
