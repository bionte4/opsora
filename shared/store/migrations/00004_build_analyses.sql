-- +goose Up
CREATE TABLE build_analyses (
    build_id     BIGINT PRIMARY KEY REFERENCES builds (id) ON DELETE CASCADE,
    provider     TEXT NOT NULL,
    model        TEXT NOT NULL,
    summary      TEXT NOT NULL,
    cause        TEXT NOT NULL DEFAULT '',
    -- build_logs IDs the model pointed at; checked against the prompt, so
    -- every ID belongs to this build.
    log_line_ids BIGINT[] NOT NULL DEFAULT '{}',
    duration_ms  INT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE build_analyses;
