package files

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medonomator/files-service/internal/domain"
)

var testClient = domain.Client{
	ID:           7,
	Slug:         "hs",
	AllowedTypes: []string{"video/*", "image/*", "application/pdf"},
	MaxFileBytes: 100 << 20,
	QuotaBytes:   1 << 30,
}

var mp4Head = append([]byte{0, 0, 0, 0x20}, []byte("ftypisom")...)

func newTestService() (*Service, *fakeRepository, *fakeStorage) {
	repository := newFakeRepository(testClient.QuotaBytes)
	storage := newFakeStorage()
	service := NewService(repository, storage, Settings{
		UploadURLTTL:       15 * time.Minute,
		DownloadURLTTL:     time.Hour,
		MaxDownloadURLTTL:  7 * 24 * time.Hour,
		ResumableThreshold: 16 << 20,
	})

	return service, repository, storage
}

func requireKind(t *testing.T, err error, kind domain.ErrorKind) {
	t.Helper()

	var known *domain.Error
	if !errors.As(err, &known) || known.Kind != kind {
		t.Fatalf("expected error kind %d, got %v", kind, err)
	}
}

func uploadRequest(sizeBytes int64) CreateUploadRequest {
	return CreateUploadRequest{
		FileName:    "Урок 1.mp4",
		ContentType: "Video/MP4",
		SizeBytes:   sizeBytes,
		Folder:      "lessons/maid",
		OwnerRef:    "group:maid",
	}
}

func TestCreateUploadSmallFileUsesSinglePut(t *testing.T) {
	service, repository, storage := newTestService()

	ticket, err := service.CreateUpload(context.Background(), testClient, uploadRequest(1<<20))
	if err != nil {
		t.Fatal(err)
	}

	if ticket.UploadMode != UploadModeSingle || ticket.Method != http.MethodPut || ticket.ChunkSizeBytes != nil {
		t.Fatalf("unexpected ticket %+v", ticket)
	}

	file := repository.files[ticket.FileID]
	if file.Status != domain.StatusPending || file.ClientID != testClient.ID || file.ContentType != "video/mp4" {
		t.Fatalf("unexpected stored file %+v", file)
	}

	if !strings.HasPrefix(file.ObjectName, "hs/lessons/maid/") || !strings.HasSuffix(file.ObjectName, ".mp4") {
		t.Fatalf("unexpected object name %s", file.ObjectName)
	}

	if file.FileName != "Урок 1.mp4" || *file.OwnerRef != "group:maid" {
		t.Fatalf("metadata not kept: %+v", file)
	}

	if call := storage.signed[0]; call.objectName != domain.StagingObjectName(file.ID) || call.sizeBytes != 1<<20 || call.contentType != "video/mp4" || call.resumable {
		t.Fatalf("unexpected sign call %+v", call)
	}
}

func TestCreateUploadLargeFileIsResumable(t *testing.T) {
	service, _, _ := newTestService()

	ticket, err := service.CreateUpload(context.Background(), testClient, uploadRequest(50<<20))
	if err != nil {
		t.Fatal(err)
	}

	if ticket.UploadMode != UploadModeResumable || ticket.Method != http.MethodPost || ticket.ChunkSizeBytes == nil {
		t.Fatalf("unexpected ticket %+v", ticket)
	}
}

func TestCreateUploadDefaultsFolder(t *testing.T) {
	service, repository, _ := newTestService()
	request := uploadRequest(10)
	request.Folder = ""

	ticket, err := service.CreateUpload(context.Background(), testClient, request)
	if err != nil {
		t.Fatal(err)
	}

	if repository.files[ticket.FileID].Folder != domain.DefaultFolder {
		t.Fatal("empty folder must fall back to the default")
	}
}

func TestCreateUploadRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateUploadRequest, *fakeRepository)
		kind   domain.ErrorKind
	}{
		{"unknown type", func(r *CreateUploadRequest, _ *fakeRepository) { r.ContentType = "text/html" }, domain.KindUnsupported},
		{"type not allowed for client", func(r *CreateUploadRequest, _ *fakeRepository) { r.ContentType = "text/csv" }, domain.KindUnsupported},
		{"zero size", func(r *CreateUploadRequest, _ *fakeRepository) { r.SizeBytes = 0 }, domain.KindInvalid},
		{"too large", func(r *CreateUploadRequest, _ *fakeRepository) { r.SizeBytes = testClient.MaxFileBytes + 1 }, domain.KindTooLarge},
		{"bad folder", func(r *CreateUploadRequest, _ *fakeRepository) { r.Folder = "../x" }, domain.KindInvalid},
		{"bad owner ref", func(r *CreateUploadRequest, _ *fakeRepository) { r.OwnerRef = "a\x00b" }, domain.KindInvalid},
		{"quota", func(_ *CreateUploadRequest, repo *fakeRepository) { repo.reserved = testClient.QuotaBytes - 5 }, domain.KindQuotaExceeded},
		{"larger than the whole quota", func(r *CreateUploadRequest, _ *fakeRepository) { r.SizeBytes = testClient.QuotaBytes + 1 }, domain.KindTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, repository, _ := newTestService()
			request := uploadRequest(10)
			tc.mutate(&request, repository)

			_, err := service.CreateUpload(context.Background(), testClient, request)
			requireKind(t, err, tc.kind)

			if len(repository.files) != 0 {
				t.Fatal("rejected uploads must not be stored")
			}
		})
	}
}

func createPending(t *testing.T, service *Service, repository *fakeRepository, sizeBytes int64) domain.File {
	t.Helper()

	ticket, err := service.CreateUpload(context.Background(), testClient, uploadRequest(sizeBytes))
	if err != nil {
		t.Fatal(err)
	}

	return repository.files[ticket.FileID]
}

func TestCompleteMarksReadyAndSignsDownload(t *testing.T) {
	service, repository, storage := newTestService()
	file := createPending(t, service, repository, int64(len(mp4Head)))
	storage.put(domain.StagingObjectName(file.ID), "video/mp4", mp4Head)

	view, err := service.Complete(context.Background(), testClient, file.ID)
	if err != nil {
		t.Fatal(err)
	}

	if view.Status != domain.StatusReady || view.SizeBytes != int64(len(mp4Head)) || view.DownloadURL == nil {
		t.Fatalf("unexpected view %+v", view)
	}

	if storage.exists(domain.StagingObjectName(file.ID)) || !storage.exists(file.ObjectName) {
		t.Fatal("verified upload must be moved from staging to its final name")
	}

	if !strings.Contains(*view.DownloadURL, "inline") {
		t.Fatalf("video must be served inline: %s", *view.DownloadURL)
	}

	again, err := service.Complete(context.Background(), testClient, file.ID)
	if err != nil || again.Status != domain.StatusReady {
		t.Fatalf("complete must be idempotent, got %+v %v", again, err)
	}
}

func TestCompleteBeforeUploadIsConflict(t *testing.T) {
	service, repository, _ := newTestService()
	file := createPending(t, service, repository, 10)

	_, err := service.Complete(context.Background(), testClient, file.ID)
	requireKind(t, err, domain.KindConflict)

	if repository.files[file.ID].Status != domain.StatusPending {
		t.Fatal("file must stay pending so the client can retry")
	}
}

func TestCompleteRejectsMismatches(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		content     []byte
		declared    int64
	}{
		{"size differs", "video/mp4", mp4Head, int64(len(mp4Head)) + 1},
		{"stored type differs", "image/jpeg", mp4Head, int64(len(mp4Head))},
		{"content is not mp4", "video/mp4", []byte("MZ\x90\x00 this is an exe!"), 20},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, repository, storage := newTestService()
			file := createPending(t, service, repository, tc.declared)
			storage.put(domain.StagingObjectName(file.ID), tc.contentType, tc.content)

			_, err := service.Complete(context.Background(), testClient, file.ID)
			requireKind(t, err, domain.KindInvalid)

			if repository.files[file.ID].Status != domain.StatusFailed {
				t.Fatal("rejected upload must be marked failed")
			}

			if storage.exists(domain.StagingObjectName(file.ID)) || storage.exists(file.ObjectName) {
				t.Fatal("rejected object must be deleted")
			}

			if repository.reserved != 0 {
				t.Fatalf("rejected upload must release its reservation, %d left", repository.reserved)
			}

			_, err = service.Complete(context.Background(), testClient, file.ID)
			requireKind(t, err, domain.KindConflict)
		})
	}
}

func TestOtherClientCannotSeeFile(t *testing.T) {
	service, repository, storage := newTestService()
	file := createPending(t, service, repository, int64(len(mp4Head)))
	storage.put(domain.StagingObjectName(file.ID), "video/mp4", mp4Head)
	stranger := testClient
	stranger.ID = 99

	_, err := service.Complete(context.Background(), stranger, file.ID)
	requireKind(t, err, domain.KindNotFound)

	_, err = service.Get(context.Background(), stranger, file.ID, 0)
	requireKind(t, err, domain.KindNotFound)

	requireKind(t, service.Delete(context.Background(), stranger, file.ID), domain.KindNotFound)

	if !storage.exists(domain.StagingObjectName(file.ID)) {
		t.Fatal("stranger must not delete the object")
	}
}

func TestGetValidatesTTLAndHidesURLForPending(t *testing.T) {
	service, repository, _ := newTestService()
	file := createPending(t, service, repository, 10)

	view, err := service.Get(context.Background(), testClient, file.ID, 0)
	if err != nil || view.DownloadURL != nil {
		t.Fatalf("pending file must not have a download url: %+v %v", view, err)
	}

	_, err = service.Get(context.Background(), testClient, file.ID, 8*24*time.Hour)
	requireKind(t, err, domain.KindInvalid)

	_, err = service.Get(context.Background(), testClient, uuid.New(), 0)
	requireKind(t, err, domain.KindNotFound)
}

func TestDeleteRemovesObjectThenRow(t *testing.T) {
	service, repository, storage := newTestService()
	file := createPending(t, service, repository, 10)

	storage.deleteErr = errors.New("gcs down")
	if err := service.Delete(context.Background(), testClient, file.ID); err == nil {
		t.Fatal("storage failure must surface")
	}

	if _, exists := repository.files[file.ID]; !exists {
		t.Fatal("row must survive when the object was not deleted")
	}

	storage.deleteErr = nil
	if err := service.Delete(context.Background(), testClient, file.ID); err != nil {
		t.Fatal(err)
	}

	if _, exists := repository.files[file.ID]; exists {
		t.Fatal("row must be removed")
	}
}

func TestCompleteDetectsReplacementDuringVerification(t *testing.T) {
	service, repository, storage := newTestService()
	file := createPending(t, service, repository, int64(len(mp4Head)))
	staging := domain.StagingObjectName(file.ID)
	storage.put(staging, "video/mp4", mp4Head)
	storage.beforeRead = func() {
		storage.beforeRead = nil
		storage.put(staging, "video/mp4", mp4Head)
	}

	_, err := service.Complete(context.Background(), testClient, file.ID)
	requireKind(t, err, domain.KindConflict)

	if storage.exists(file.ObjectName) || repository.files[file.ID].Status != domain.StatusPending {
		t.Fatal("replaced content must not be promoted")
	}

	if _, err := service.Complete(context.Background(), testClient, file.ID); err != nil {
		t.Fatalf("retry must verify the new generation: %v", err)
	}
}

func TestCompleteRecoversAfterPromoteWithoutMarkReady(t *testing.T) {
	service, repository, storage := newTestService()
	file := createPending(t, service, repository, int64(len(mp4Head)))
	storage.put(file.ObjectName, "video/mp4", mp4Head)

	view, err := service.Complete(context.Background(), testClient, file.ID)
	if err != nil || view.Status != domain.StatusReady {
		t.Fatalf("promoted object must be accepted on retry: %+v %v", view, err)
	}
}

func TestCompleteAfterJanitorClaimIsConflict(t *testing.T) {
	service, repository, storage := newTestService()
	file := createPending(t, service, repository, int64(len(mp4Head)))
	storage.put(domain.StagingObjectName(file.ID), "video/mp4", mp4Head)
	storage.beforeRead = func() {
		if _, err := repository.MarkFailed(context.Background(), file.ID); err != nil {
			t.Fatal(err)
		}
	}

	_, err := service.Complete(context.Background(), testClient, file.ID)
	requireKind(t, err, domain.KindConflict)
}

func TestDeleteReleasesReservation(t *testing.T) {
	service, repository, _ := newTestService()
	file := createPending(t, service, repository, 1000)

	if err := service.Delete(context.Background(), testClient, file.ID); err != nil {
		t.Fatal(err)
	}

	if repository.reserved != 0 {
		t.Fatalf("delete must release the reservation, %d left", repository.reserved)
	}
}
