-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

ALTER TABLE build_analyses
    ADD COLUMN embedding vector(768);

-- +goose Down
ALTER TABLE build_analyses DROP COLUMN embedding;
