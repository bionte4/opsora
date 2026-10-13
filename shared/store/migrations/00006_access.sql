-- +goose Up
CREATE TABLE roles (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    system      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE permissions (
    key TEXT PRIMARY KEY
);

CREATE TABLE role_permissions (
    role_id    BIGINT NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    permission TEXT NOT NULL REFERENCES permissions (key),
    PRIMARY KEY (role_id, permission)
);

CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role_id       BIGINT NOT NULL REFERENCES roles (id),
    disabled      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX sessions_user_idx ON sessions (user_id);

-- +goose Down
DROP TABLE sessions;
DROP TABLE users;
DROP TABLE role_permissions;
DROP TABLE permissions;
DROP TABLE roles;
