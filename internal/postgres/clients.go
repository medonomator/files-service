package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/medonomator/files-service/internal/domain"
)

const clientColumns = `c.id, c.slug, c.name, c.allowed_types, c.max_file_bytes, c.quota_bytes, c.rate_limit_per_minute`

func scanClient(row pgx.Row) (domain.Client, error) {
	var client domain.Client

	err := row.Scan(
		&client.ID, &client.Slug, &client.Name, &client.AllowedTypes,
		&client.MaxFileBytes, &client.QuotaBytes, &client.RateLimitPerMinute,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Client{}, domain.ErrNotFound
	}

	return client, err
}

func (s *Store) ClientByKeyHash(ctx context.Context, hash []byte) (domain.Client, error) {
	return scanClient(s.pool.QueryRow(ctx, `
		SELECT `+clientColumns+`
		FROM api_keys k JOIN clients c ON c.id = k.client_id
		WHERE k.key_hash = $1 AND k.revoked_at IS NULL AND c.disabled_at IS NULL`, hash,
	))
}

func (s *Store) ClientBySlug(ctx context.Context, slug string) (domain.Client, error) {
	return scanClient(s.pool.QueryRow(ctx, `SELECT `+clientColumns+` FROM clients c WHERE c.slug = $1`, slug))
}

func (s *Store) CreateClient(ctx context.Context, client domain.Client) (domain.Client, error) {
	return scanClient(s.pool.QueryRow(ctx, `
		INSERT INTO clients AS c (slug, name, allowed_types, max_file_bytes, quota_bytes, rate_limit_per_minute)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+clientColumns,
		client.Slug, client.Name, client.AllowedTypes, client.MaxFileBytes, client.QuotaBytes, client.RateLimitPerMinute,
	))
}

func (s *Store) SetClientDisabled(ctx context.Context, slug string, disabled bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE clients SET disabled_at = CASE WHEN $2 THEN now() END WHERE slug = $1`, slug, disabled,
	)
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return err
}

func (s *Store) AddKey(ctx context.Context, clientID int64, hash []byte, display string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO api_keys (client_id, key_hash, display) VALUES ($1, $2, $3)`, clientID, hash, display,
	)

	return err
}

func (s *Store) RevokeKey(ctx context.Context, display string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE display = $1 AND revoked_at IS NULL`, display,
	)
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	return err
}

type ClientSummary struct {
	Client     domain.Client
	Disabled   bool
	ActiveKeys []string
	UsedBytes  int64
	FileCount  int64
	CreatedAt  time.Time
}

func (s *Store) ListClients(ctx context.Context) ([]ClientSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+clientColumns+`, c.disabled_at IS NOT NULL, c.created_at,
			COALESCE((SELECT array_agg(k.display ORDER BY k.created_at) FROM api_keys k
				WHERE k.client_id = c.id AND k.revoked_at IS NULL), '{}'),
			c.reserved_bytes,
			(SELECT count(*) FROM files f WHERE f.client_id = c.id AND f.status = 'ready')
		FROM clients c ORDER BY c.id`)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var summaries []ClientSummary
	for rows.Next() {
		var summary ClientSummary
		client := &summary.Client

		if err := rows.Scan(
			&client.ID, &client.Slug, &client.Name, &client.AllowedTypes,
			&client.MaxFileBytes, &client.QuotaBytes, &client.RateLimitPerMinute,
			&summary.Disabled, &summary.CreatedAt, &summary.ActiveKeys, &summary.UsedBytes, &summary.FileCount,
		); err != nil {
			return nil, err
		}

		summaries = append(summaries, summary)
	}

	return summaries, rows.Err()
}
