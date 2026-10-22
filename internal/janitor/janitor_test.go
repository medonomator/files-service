package janitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medonomator/files-service/internal/domain"
)

type fakeRepository struct {
	files map[uuid.UUID]domain.File
	ages  map[uuid.UUID]time.Duration
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{files: map[uuid.UUID]domain.File{}, ages: map[uuid.UUID]time.Duration{}}
}

func (r *fakeRepository) add(status domain.Status, age time.Duration) domain.File {
	id := uuid.New()
	file := domain.File{ID: id, Status: status, ObjectName: "hs/files/" + id.String() + ".mp4"}
	r.files[id] = file
	r.ages[id] = age

	return file
}

func (r *fakeRepository) UnfinishedOlderThan(_ context.Context, age time.Duration, limit int) ([]domain.File, error) {
	var stale []domain.File
	for id, file := range r.files {
		if file.Status != domain.StatusReady && r.ages[id] > age && len(stale) < limit {
			stale = append(stale, file)
		}
	}

	return stale, nil
}

func (r *fakeRepository) MarkFailed(_ context.Context, id uuid.UUID) (bool, error) {
	file, ok := r.files[id]
	if !ok || file.Status != domain.StatusPending {
		return false, nil
	}

	file.Status = domain.StatusFailed
	r.files[id] = file

	return true, nil
}

func (r *fakeRepository) DeleteFailed(_ context.Context, id uuid.UUID) error {
	if r.files[id].Status == domain.StatusFailed {
		delete(r.files, id)
	}

	return nil
}

type fakeRemover struct {
	removed []string
	err     error
}

func (r *fakeRemover) Delete(_ context.Context, objectName string) error {
	if r.err != nil {
		return r.err
	}

	r.removed = append(r.removed, objectName)

	return nil
}

func newJanitor(repository *fakeRepository, remover *fakeRemover) *Janitor {
	return New(repository, remover, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestSweepRemovesOnlyOldUnfinished(t *testing.T) {
	repository := newFakeRepository()
	oldPending := repository.add(domain.StatusPending, 25*time.Hour)
	oldFailed := repository.add(domain.StatusFailed, 30*time.Hour)
	freshPending := repository.add(domain.StatusPending, time.Hour)
	oldReady := repository.add(domain.StatusReady, 100*time.Hour)
	remover := &fakeRemover{}

	result, err := newJanitor(repository, remover).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if result.Removed != 2 || result.Failed != 0 || len(repository.files) != 2 {
		t.Fatalf("unexpected result %+v, left %d", result, len(repository.files))
	}

	for _, kept := range []domain.File{freshPending, oldReady} {
		if _, ok := repository.files[kept.ID]; !ok {
			t.Fatalf("%s must be kept", kept.Status)
		}
	}

	want := map[string]bool{
		domain.StagingObjectName(oldPending.ID): true, oldPending.ObjectName: true,
		domain.StagingObjectName(oldFailed.ID): true, oldFailed.ObjectName: true,
	}
	if len(remover.removed) != len(want) {
		t.Fatalf("unexpected removed objects %v", remover.removed)
	}

	for _, name := range remover.removed {
		if !want[name] {
			t.Fatalf("unexpected removed object %s", name)
		}
	}
}

func TestSweepClaimsBeforeDeletingObjects(t *testing.T) {
	repository := newFakeRepository()
	file := repository.add(domain.StatusPending, 48*time.Hour)
	remover := &fakeRemover{err: errors.New("gcs down")}

	result, err := newJanitor(repository, remover).Sweep(context.Background())
	if err != nil || result.Failed != 1 || result.Removed != 0 {
		t.Fatalf("unexpected result %+v %v", result, err)
	}

	if repository.files[file.ID].Status != domain.StatusFailed {
		t.Fatal("row must be claimed so complete can no longer race the cleanup")
	}

	remover.err = nil

	result, err = newJanitor(repository, remover).Sweep(context.Background())
	if err != nil || result.Removed != 1 || len(repository.files) != 0 {
		t.Fatalf("next sweep must finish the cleanup: %+v %v", result, err)
	}
}

func TestSweepSkipsUploadCompletedMeanwhile(t *testing.T) {
	repository := newFakeRepository()
	file := repository.add(domain.StatusPending, 48*time.Hour)
	remover := &fakeRemover{}
	janitor := newJanitor(repository, remover)

	stale, _ := repository.UnfinishedOlderThan(context.Background(), time.Hour, 10)
	ready := repository.files[file.ID]
	ready.Status = domain.StatusReady
	repository.files[file.ID] = ready

	removed, err := janitor.remove(context.Background(), stale[0])
	if err != nil || removed || len(remover.removed) != 0 {
		t.Fatalf("completed upload must be left alone: %v %v %v", removed, err, remover.removed)
	}
}

func TestOnSweepReportsResult(t *testing.T) {
	repository := newFakeRepository()
	repository.add(domain.StatusFailed, 48*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	var observed Result

	newJanitor(repository, &fakeRemover{}).OnSweep(func(result Result) {
		observed = result
		cancel()
	}).Run(ctx, time.Hour)

	if observed.Removed != 1 {
		t.Fatalf("observer got %+v", observed)
	}
}
