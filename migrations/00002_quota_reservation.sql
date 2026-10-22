-- +goose Up
ALTER TABLE clients ADD COLUMN reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0);

UPDATE clients c SET reserved_bytes = COALESCE((
    SELECT SUM(f.declared_size_bytes) FROM files f WHERE f.client_id = c.id AND f.status <> 'failed'
), 0);

ALTER TABLE api_keys ADD CONSTRAINT api_keys_display_key UNIQUE (display);

ALTER TABLE files
    ADD CONSTRAINT files_size_positive CHECK (size_bytes IS NULL OR size_bytes > 0),
    ADD CONSTRAINT files_ready_complete CHECK (status <> 'ready' OR (size_bytes IS NOT NULL AND ready_at IS NOT NULL)),
    ADD CONSTRAINT files_folder_format CHECK (folder ~ '^[a-z0-9][a-z0-9_-]{0,62}(/[a-z0-9][a-z0-9_-]{0,62}){0,3}$');

CREATE INDEX files_owner_idx ON files (client_id, owner_ref, created_at) WHERE owner_ref IS NOT NULL;

-- +goose Down
DROP INDEX files_owner_idx;

ALTER TABLE files
    DROP CONSTRAINT files_folder_format,
    DROP CONSTRAINT files_ready_complete,
    DROP CONSTRAINT files_size_positive;

ALTER TABLE api_keys DROP CONSTRAINT api_keys_display_key;

ALTER TABLE clients DROP COLUMN reserved_bytes;
