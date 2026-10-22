package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medonomator/files-service/internal/domain"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

const fileColumns = `id, client_id, status, folder, owner_ref, file_name, object_name,
	content_type, declared_size_bytes, size_bytes, created_at, ready_at`

func scanFile(row pgx.Row) (domain.File, error) {
	var file domain.File

	err := row.Scan(
		&file.ID, &file.ClientID, &file.Status, &file.Folder, &file.OwnerRef, &file.FileName, &file.ObjectName,
		&file.ContentType, &file.DeclaredSizeBytes, &file.SizeBytes, &file.CreatedAt, &file.ReadyAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.File{}, domain.ErrNotFound
	}

	return file, err
}

func (s *Store) CreateFile(ctx context.Context, file domain.File) (domain.File, error) {
	created, err := scanFile(s.pool.QueryRow(ctx, `
		WITH reservation AS (
			UPDATE clients SET reserved_bytes = reserved_bytes + $9
			WHERE id = $2 AND reserved_bytes + $9 <= quota_bytes
			RETURNING id
		)
		INSERT INTO files (id, client_id, status, folder, owner_ref, file_name, object_name, content_type, declared_size_bytes)
		SELECT $1, reservation.id, $3, $4, $5, $6, $7, $8, $9 FROM reservation
		RETURNING `+fileColumns,
		file.ID, file.ClientID, file.Status, file.Folder, file.OwnerRef, file.FileName, file.ObjectName,
		file.ContentType, file.DeclaredSizeBytes,
	))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.File{}, domain.ErrQuotaExceeded
	}

	if err != nil {
		return domain.File{}, fmt.Errorf("insert file %s: %w", file.ID, err)
	}

	return created, nil
}

func (s *Store) FileByID(ctx context.Context, clientID int64, id uuid.UUID) (domain.File, error) {
	file, err := scanFile(s.pool.QueryRow(ctx,
		`SELECT `+fileColumns+` FROM files WHERE id = $1 AND client_id = $2`, id, clientID,
	))
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.File{}, fmt.Errorf("select file %s: %w", id, err)
	}

	return file, err
}

func (s *Store) MarkReady(ctx context.Context, id uuid.UUID, sizeBytes int64) (domain.File, error) {
	file, err := scanFile(s.pool.QueryRow(ctx, `
		UPDATE files SET status = 'ready', size_bytes = $2, ready_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING `+fileColumns,
		id, sizeBytes,
	))
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.File{}, fmt.Errorf("mark %s ready: %w", id, err)
	}

	return file, err
}

func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH failed AS (
			UPDATE files SET status = 'failed'
			WHERE id = $1 AND status = 'pending'
			RETURNING client_id, declared_size_bytes
		)
		UPDATE clients c SET reserved_bytes = c.reserved_bytes - failed.declared_size_bytes
		FROM failed WHERE c.id = failed.client_id`, id,
	)
	if err != nil {
		return false, fmt.Errorf("mark %s failed: %w", id, err)
	}

	return tag.RowsAffected() > 0, nil
}

func (s *Store) DeleteFile(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		WITH removed AS (
			DELETE FROM files WHERE id = $1
			RETURNING client_id, status, declared_size_bytes
		)
		UPDATE clients c SET reserved_bytes = c.reserved_bytes - removed.declared_size_bytes
		FROM removed WHERE c.id = removed.client_id AND removed.status <> 'failed'`, id,
	)
	if err != nil {
		return fmt.Errorf("delete file %s: %w", id, err)
	}

	return nil
}

func (s *Store) DeleteFailed(ctx context.Context, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM files WHERE id = $1 AND status = 'failed'`, id); err != nil {
		return fmt.Errorf("delete failed file %s: %w", id, err)
	}

	return nil
}

func (s *Store) UnfinishedOlderThan(ctx context.Context, age time.Duration, limit int) ([]domain.File, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+fileColumns+` FROM files
		WHERE status <> 'ready' AND created_at < now() - make_interval(secs => $1)
		ORDER BY created_at
		LIMIT $2`, age.Seconds(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select unfinished files: %w", err)
	}

	defer rows.Close()

	var stale []domain.File
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan unfinished file: %w", err)
		}

		stale = append(stale, file)
	}

	return stale, rows.Err()
}
