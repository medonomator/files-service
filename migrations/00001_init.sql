-- +goose Up
CREATE TABLE clients (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,31}$'),
    name text NOT NULL,
    allowed_types text[] NOT NULL CHECK (cardinality(allowed_types) > 0),
    max_file_bytes bigint NOT NULL CHECK (max_file_bytes > 0),
    quota_bytes bigint NOT NULL CHECK (quota_bytes > 0),
    rate_limit_per_minute integer NOT NULL DEFAULT 120 CHECK (rate_limit_per_minute > 0),
    disabled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    client_id bigint NOT NULL REFERENCES clients (id) ON DELETE CASCADE,
    key_hash bytea NOT NULL UNIQUE,
    display text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

CREATE INDEX api_keys_client_idx ON api_keys (client_id);

CREATE TABLE files (
    id uuid PRIMARY KEY,
    client_id bigint NOT NULL REFERENCES clients (id),
    status text NOT NULL CHECK (status IN ('pending', 'ready', 'failed')),
    folder text NOT NULL,
    owner_ref text,
    file_name text NOT NULL,
    object_name text NOT NULL UNIQUE,
    content_type text NOT NULL,
    declared_size_bytes bigint NOT NULL CHECK (declared_size_bytes > 0),
    size_bytes bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    ready_at timestamptz
);

CREATE INDEX files_client_idx ON files (client_id, status);
CREATE INDEX files_unfinished_idx ON files (created_at) WHERE status <> 'ready';

-- +goose Down
DROP TABLE files;
DROP TABLE api_keys;
DROP TABLE clients;
