-- +goose Up
ALTER TABLE builds ADD COLUMN notified_at TIMESTAMPTZ;

-- Builds that finished before notifications existed are not announced.
UPDATE builds SET notified_at = now() WHERE status IN ('success', 'failed');

CREATE INDEX builds_unnotified_idx ON builds (finished_at)
    WHERE notified_at IS NULL AND status IN ('success', 'failed');

-- +goose Down
DROP INDEX builds_unnotified_idx;
ALTER TABLE builds DROP COLUMN notified_at;
