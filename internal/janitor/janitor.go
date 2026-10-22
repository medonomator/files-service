package janitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/medonomator/files-service/internal/domain"
)

const batchSize = 200

type Repository interface {
	UnfinishedOlderThan(ctx context.Context, age time.Duration, limit int) ([]domain.File, error)
	MarkFailed(ctx context.Context, id uuid.UUID) (bool, error)
	DeleteFailed(ctx context.Context, id uuid.UUID) error
}

type ObjectRemover interface {
	Delete(ctx context.Context, objectName string) error
}

type Result struct {
	Removed int
	Failed  int
}

type Janitor struct {
	repository Repository
	storage    ObjectRemover
	pendingTTL time.Duration
	logger     *slog.Logger
	observe    func(Result)
}

func New(repository Repository, storage ObjectRemover, pendingTTL time.Duration, logger *slog.Logger) *Janitor {
	return &Janitor{repository: repository, storage: storage, pendingTTL: pendingTTL, logger: logger, observe: func(Result) {}}
}

func (j *Janitor) OnSweep(observe func(Result)) *Janitor {
	j.observe = observe

	return j
}

func (j *Janitor) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		result, err := j.Sweep(ctx)

		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			j.logger.ErrorContext(ctx, "janitor sweep failed", "error", err, "removed", result.Removed, "failed", result.Failed)
		case result.Removed > 0 || result.Failed > 0:
			j.logger.InfoContext(ctx, "janitor swept unfinished uploads", "removed", result.Removed, "failed", result.Failed)
		}

		j.observe(result)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (j *Janitor) Sweep(ctx context.Context) (Result, error) {
	var result Result

	for ctx.Err() == nil {
		stale, err := j.repository.UnfinishedOlderThan(ctx, j.pendingTTL, batchSize)
		if err != nil {
			return result, err
		}

		failedBefore := result.Failed

		for _, file := range stale {
			removed, err := j.remove(ctx, file)
			if err != nil {
				result.Failed++
				j.logger.WarnContext(ctx, "janitor could not remove upload", "file", file.ID, "error", err)

				continue
			}

			if removed {
				result.Removed++
			}
		}

		if len(stale) < batchSize || result.Failed > failedBefore {
			break
		}
	}

	return result, nil
}

func (j *Janitor) remove(ctx context.Context, file domain.File) (bool, error) {
	if file.Status == domain.StatusPending {
		claimed, err := j.repository.MarkFailed(ctx, file.ID)
		if err != nil || !claimed {
			return false, err
		}
	}

	for _, objectName := range []string{domain.StagingObjectName(file.ID), file.ObjectName} {
		if err := j.storage.Delete(ctx, objectName); err != nil {
			return false, err
		}
	}

	return true, j.repository.DeleteFailed(ctx, file.ID)
}
