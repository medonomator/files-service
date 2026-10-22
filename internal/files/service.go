package files

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/medonomator/files-service/internal/domain"
)

type Repository interface {
	CreateFile(ctx context.Context, file domain.File) (domain.File, error)
	FileByID(ctx context.Context, clientID int64, id uuid.UUID) (domain.File, error)
	MarkReady(ctx context.Context, id uuid.UUID, sizeBytes int64) (domain.File, error)
	MarkFailed(ctx context.Context, id uuid.UUID) (bool, error)
	DeleteFile(ctx context.Context, id uuid.UUID) error
}

type Storage interface {
	SignUpload(objectName, contentType string, sizeBytes int64, resumable bool, ttl time.Duration) (domain.SignedRequest, error)
	SignDownload(objectName, disposition string, ttl time.Duration) (string, error)
	Stat(ctx context.Context, objectName string) (domain.StoredObject, error)
	ReadHead(ctx context.Context, objectName string, generation, length int64) ([]byte, error)
	Promote(ctx context.Context, source string, generation int64, target string) error
	Delete(ctx context.Context, objectName string) error
}

type Metrics interface {
	UploadCreated(client, mode string)
	UploadCompleted(client string, sizeBytes int64)
	UploadRejected(client, reason string)
}

type noMetrics struct{}

func (noMetrics) UploadCreated(string, string)  {}
func (noMetrics) UploadCompleted(string, int64) {}
func (noMetrics) UploadRejected(string, string) {}

type Settings struct {
	UploadURLTTL       time.Duration
	DownloadURLTTL     time.Duration
	MaxDownloadURLTTL  time.Duration
	ResumableThreshold int64
}

const (
	UploadModeSingle    = "single"
	UploadModeResumable = "resumable"
	resumableChunkBytes = 8 << 20
)

type Service struct {
	repository Repository
	storage    Storage
	settings   Settings
	metrics    Metrics
	now        func() time.Time
}

func NewService(repository Repository, storage Storage, settings Settings) *Service {
	return &Service{repository: repository, storage: storage, settings: settings, metrics: noMetrics{}, now: time.Now}
}

func (s *Service) WithMetrics(metrics Metrics) *Service {
	s.metrics = metrics

	return s
}

type CreateUploadRequest struct {
	FileName    string
	ContentType string
	SizeBytes   int64
	Folder      string
	OwnerRef    string
}

type UploadTicket struct {
	FileID         uuid.UUID         `json:"fileId"`
	UploadMode     string            `json:"uploadMode"`
	Method         string            `json:"method"`
	UploadURL      string            `json:"uploadUrl"`
	Headers        map[string]string `json:"headers"`
	ChunkSizeBytes *int              `json:"chunkSizeBytes,omitempty"`
	ExpiresAt      time.Time         `json:"expiresAt"`
}

type View struct {
	ID                  uuid.UUID     `json:"id"`
	Status              domain.Status `json:"status"`
	Folder              string        `json:"folder"`
	OwnerRef            *string       `json:"ownerRef"`
	FileName            string        `json:"fileName"`
	ContentType         string        `json:"contentType"`
	SizeBytes           int64         `json:"sizeBytes"`
	CreatedAt           time.Time     `json:"createdAt"`
	ReadyAt             *time.Time    `json:"readyAt"`
	DownloadURL         *string       `json:"downloadUrl"`
	DownloadURLExpireAt *time.Time    `json:"downloadUrlExpiresAt"`
}

func (s *Service) CreateUpload(ctx context.Context, client domain.Client, request CreateUploadRequest) (UploadTicket, error) {
	contentType := domain.NormalizeContentType(request.ContentType)

	kind, ok := domain.LookupContentType(contentType)
	if !ok {
		return UploadTicket{}, domain.Unsupported("Content type %q is not supported", request.ContentType)
	}

	if !client.Allows(contentType) {
		return UploadTicket{}, domain.Unsupported("Content type %q is not allowed for this client", contentType)
	}

	if request.SizeBytes < 1 {
		return UploadTicket{}, domain.Invalid("sizeBytes must be positive")
	}

	if request.SizeBytes > client.MaxFileBytes {
		return UploadTicket{}, domain.TooLarge("File exceeds the %d byte limit", client.MaxFileBytes)
	}

	folder := request.Folder
	if folder == "" {
		folder = domain.DefaultFolder
	}

	if !domain.ValidFolder(folder) {
		return UploadTicket{}, domain.Invalid("folder must be up to 4 segments of a-z, 0-9, _ and -")
	}

	ownerRef, ok := domain.NormalizeOwnerRef(request.OwnerRef)
	if !ok {
		return UploadTicket{}, domain.Invalid("ownerRef must be up to 128 printable characters")
	}

	if request.SizeBytes > client.QuotaBytes {
		return UploadTicket{}, domain.QuotaExceeded("Storage quota of %d bytes is exhausted", client.QuotaBytes)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return UploadTicket{}, fmt.Errorf("generate file id: %w", err)
	}

	resumable := request.SizeBytes >= s.settings.ResumableThreshold

	signed, err := s.storage.SignUpload(domain.StagingObjectName(id), contentType, request.SizeBytes, resumable, s.settings.UploadURLTTL)
	if err != nil {
		return UploadTicket{}, err
	}

	_, err = s.repository.CreateFile(ctx, domain.File{
		ID:                id,
		ClientID:          client.ID,
		Status:            domain.StatusPending,
		Folder:            folder,
		OwnerRef:          ownerRef,
		FileName:          domain.WithExtension(domain.CleanFileName(request.FileName), kind.Extension, kind.Aliases...),
		ObjectName:        domain.ObjectName(client.Slug, folder, id, kind.Extension),
		ContentType:       contentType,
		DeclaredSizeBytes: request.SizeBytes,
	})
	if errors.Is(err, domain.ErrQuotaExceeded) {
		return UploadTicket{}, domain.QuotaExceeded("Storage quota of %d bytes is exhausted", client.QuotaBytes)
	}

	if err != nil {
		return UploadTicket{}, err
	}

	ticket := UploadTicket{
		FileID:     id,
		UploadMode: UploadModeSingle,
		Method:     signed.Method,
		UploadURL:  signed.URL,
		Headers:    signed.Headers,
		ExpiresAt:  signed.ExpiresAt.UTC(),
	}

	if resumable {
		chunk := resumableChunkBytes
		ticket.UploadMode = UploadModeResumable
		ticket.ChunkSizeBytes = &chunk
	}

	s.metrics.UploadCreated(client.Slug, ticket.UploadMode)

	return ticket, nil
}

func (s *Service) Complete(ctx context.Context, client domain.Client, id uuid.UUID) (View, error) {
	file, err := s.find(ctx, client, id)
	if err != nil {
		return View{}, err
	}

	switch file.Status {
	case domain.StatusReady:
		return s.view(file, s.settings.DownloadURLTTL)
	case domain.StatusFailed:
		return View{}, domain.Conflict("Upload was rejected or expired, start a new one")
	}

	ctx = context.WithoutCancel(ctx)
	staging := domain.StagingObjectName(file.ID)

	object, err := s.storage.Stat(ctx, staging)
	if errors.Is(err, domain.ErrNotFound) {
		return s.completePromoted(ctx, client, file)
	}

	if err != nil {
		return View{}, err
	}

	reason, err := s.verify(ctx, file, staging, object)
	if reason != "" {
		s.metrics.UploadRejected(client.Slug, reason)
		return View{}, s.reject(ctx, file, staging, err)
	}

	if err != nil {
		return View{}, err
	}

	err = s.storage.Promote(ctx, staging, object.Generation, file.ObjectName)
	if errors.Is(err, domain.ErrNotFound) {
		return View{}, domain.Conflict("File changed while it was being verified, call complete again")
	}

	if err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
		return View{}, err
	}

	return s.markReady(ctx, client, file, object.SizeBytes)
}

func (s *Service) verify(ctx context.Context, file domain.File, staging string, object domain.StoredObject) (string, error) {
	if object.SizeBytes != file.DeclaredSizeBytes {
		return "size_mismatch", domain.Invalid("Uploaded %d bytes but declared %d", object.SizeBytes, file.DeclaredSizeBytes)
	}

	if domain.NormalizeContentType(object.ContentType) != file.ContentType {
		return "type_mismatch", domain.Invalid("Uploaded content type does not match %s", file.ContentType)
	}

	head, err := s.storage.ReadHead(ctx, staging, object.Generation, domain.SniffBytes)
	if errors.Is(err, domain.ErrNotFound) {
		return "", domain.Conflict("File changed while it was being verified, call complete again")
	}

	if err != nil {
		return "", err
	}

	if !domain.MatchesContent(file.ContentType, head) {
		return "signature_mismatch", domain.Invalid("File content is not %s", file.ContentType)
	}

	return "", nil
}

func (s *Service) completePromoted(ctx context.Context, client domain.Client, file domain.File) (View, error) {
	promoted, err := s.storage.Stat(ctx, file.ObjectName)
	if errors.Is(err, domain.ErrNotFound) {
		return View{}, domain.Conflict("File has not been uploaded yet")
	}

	if err != nil {
		return View{}, err
	}

	return s.markReady(ctx, client, file, promoted.SizeBytes)
}

func (s *Service) markReady(ctx context.Context, client domain.Client, file domain.File, sizeBytes int64) (View, error) {
	ready, err := s.repository.MarkReady(ctx, file.ID, sizeBytes)
	if errors.Is(err, domain.ErrNotFound) {
		ready, err = s.find(ctx, client, file.ID)
		if err == nil && ready.Status != domain.StatusReady {
			return View{}, domain.Conflict("Upload expired before it was completed, start a new one")
		}
	} else if err == nil {
		s.metrics.UploadCompleted(client.Slug, sizeBytes)
	}

	if err != nil {
		return View{}, err
	}

	return s.view(ready, s.settings.DownloadURLTTL)
}

func (s *Service) reject(ctx context.Context, file domain.File, staging string, reason error) error {
	if err := s.storage.Delete(ctx, staging); err != nil {
		return err
	}

	if _, err := s.repository.MarkFailed(ctx, file.ID); err != nil {
		return err
	}

	return reason
}

func (s *Service) Get(ctx context.Context, client domain.Client, id uuid.UUID, urlTTL time.Duration) (View, error) {
	if urlTTL == 0 {
		urlTTL = s.settings.DownloadURLTTL
	}

	if urlTTL < time.Second || urlTTL > s.settings.MaxDownloadURLTTL {
		return View{}, domain.Invalid("urlTtl must be between 1 and %d seconds", int(s.settings.MaxDownloadURLTTL.Seconds()))
	}

	file, err := s.find(ctx, client, id)
	if err != nil {
		return View{}, err
	}

	return s.view(file, urlTTL)
}

func (s *Service) Delete(ctx context.Context, client domain.Client, id uuid.UUID) error {
	file, err := s.find(ctx, client, id)
	if err != nil {
		return err
	}

	ctx = context.WithoutCancel(ctx)

	for _, objectName := range []string{file.ObjectName, domain.StagingObjectName(file.ID)} {
		if err := s.storage.Delete(ctx, objectName); err != nil {
			return err
		}
	}

	return s.repository.DeleteFile(ctx, file.ID)
}

func (s *Service) find(ctx context.Context, client domain.Client, id uuid.UUID) (domain.File, error) {
	file, err := s.repository.FileByID(ctx, client.ID, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.File{}, domain.NotFound("File not found")
	}

	return file, err
}

func (s *Service) view(file domain.File, urlTTL time.Duration) (View, error) {
	view := View{
		ID:          file.ID,
		Status:      file.Status,
		Folder:      file.Folder,
		OwnerRef:    file.OwnerRef,
		FileName:    file.FileName,
		ContentType: file.ContentType,
		SizeBytes:   file.DeclaredSizeBytes,
		CreatedAt:   file.CreatedAt.UTC(),
		ReadyAt:     file.ReadyAt,
	}

	if file.SizeBytes != nil {
		view.SizeBytes = *file.SizeBytes
	}

	if file.Status != domain.StatusReady {
		return view, nil
	}

	kind, _ := domain.LookupContentType(file.ContentType)
	expiresAt := s.now().Add(urlTTL).UTC()

	downloadURL, err := s.storage.SignDownload(file.ObjectName, domain.ContentDisposition(kind.Inline, file.FileName), urlTTL)
	if err != nil {
		return View{}, err
	}

	view.DownloadURL = &downloadURL
	view.DownloadURLExpireAt = &expiresAt

	return view, nil
}
