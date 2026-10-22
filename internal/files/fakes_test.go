package files

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/medonomator/files-service/internal/domain"
)

type fakeRepository struct {
	files    map[uuid.UUID]domain.File
	quota    int64
	reserved int64
}

func newFakeRepository(quota int64) *fakeRepository {
	return &fakeRepository{files: map[uuid.UUID]domain.File{}, quota: quota}
}

func (r *fakeRepository) CreateFile(_ context.Context, file domain.File) (domain.File, error) {
	if r.reserved+file.DeclaredSizeBytes > r.quota {
		return domain.File{}, domain.ErrQuotaExceeded
	}

	r.reserved += file.DeclaredSizeBytes
	file.CreatedAt = time.Now()
	r.files[file.ID] = file

	return file, nil
}

func (r *fakeRepository) FileByID(_ context.Context, clientID int64, id uuid.UUID) (domain.File, error) {
	file, ok := r.files[id]
	if !ok || file.ClientID != clientID {
		return domain.File{}, domain.ErrNotFound
	}

	return file, nil
}

func (r *fakeRepository) MarkReady(_ context.Context, id uuid.UUID, sizeBytes int64) (domain.File, error) {
	file, ok := r.files[id]
	if !ok || file.Status != domain.StatusPending {
		return domain.File{}, domain.ErrNotFound
	}

	now := time.Now()
	file.Status = domain.StatusReady
	file.SizeBytes = &sizeBytes
	file.ReadyAt = &now
	r.files[id] = file

	return file, nil
}

func (r *fakeRepository) MarkFailed(_ context.Context, id uuid.UUID) (bool, error) {
	file, ok := r.files[id]
	if !ok || file.Status != domain.StatusPending {
		return false, nil
	}

	file.Status = domain.StatusFailed
	r.files[id] = file
	r.reserved -= file.DeclaredSizeBytes

	return true, nil
}

func (r *fakeRepository) DeleteFile(_ context.Context, id uuid.UUID) error {
	if file, ok := r.files[id]; ok && file.Status != domain.StatusFailed {
		r.reserved -= file.DeclaredSizeBytes
	}

	delete(r.files, id)

	return nil
}

type signCall struct {
	objectName  string
	contentType string
	sizeBytes   int64
	resumable   bool
}

type storedBlob struct {
	object  domain.StoredObject
	content []byte
}

type fakeStorage struct {
	blobs      map[string]storedBlob
	generation int64
	signed     []signCall
	deleteErr  error
	beforeRead func()
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{blobs: map[string]storedBlob{}}
}

func (s *fakeStorage) put(objectName, contentType string, content []byte) {
	s.generation++
	s.blobs[objectName] = storedBlob{
		object:  domain.StoredObject{SizeBytes: int64(len(content)), ContentType: contentType, Generation: s.generation},
		content: content,
	}
}

func (s *fakeStorage) exists(objectName string) bool {
	_, ok := s.blobs[objectName]

	return ok
}

func (s *fakeStorage) SignUpload(objectName, contentType string, sizeBytes int64, resumable bool, ttl time.Duration) (domain.SignedRequest, error) {
	s.signed = append(s.signed, signCall{objectName, contentType, sizeBytes, resumable})
	method := "PUT"
	if resumable {
		method = "POST"
	}

	return domain.SignedRequest{
		Method:    method,
		URL:       "https://storage.test/" + objectName,
		Headers:   map[string]string{"Content-Type": contentType},
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

func (s *fakeStorage) SignDownload(objectName, disposition string, _ time.Duration) (string, error) {
	return "https://storage.test/" + objectName + "?disposition=" + disposition, nil
}

func (s *fakeStorage) Stat(_ context.Context, objectName string) (domain.StoredObject, error) {
	blob, ok := s.blobs[objectName]
	if !ok {
		return domain.StoredObject{}, domain.ErrNotFound
	}

	return blob.object, nil
}

func (s *fakeStorage) ReadHead(_ context.Context, objectName string, generation, length int64) ([]byte, error) {
	if s.beforeRead != nil {
		s.beforeRead()
	}

	blob, ok := s.blobs[objectName]
	if !ok || blob.object.Generation != generation {
		return nil, domain.ErrNotFound
	}

	head := blob.content
	if int64(len(head)) > length {
		head = head[:length]
	}

	return head, nil
}

func (s *fakeStorage) Promote(_ context.Context, source string, generation int64, target string) error {
	blob, ok := s.blobs[source]
	if !ok || blob.object.Generation != generation {
		if s.exists(target) {
			return domain.ErrAlreadyExists
		}

		return domain.ErrNotFound
	}

	if s.exists(target) {
		return domain.ErrAlreadyExists
	}

	s.blobs[target] = blob
	delete(s.blobs, source)

	return nil
}

func (s *fakeStorage) Delete(_ context.Context, objectName string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}

	delete(s.blobs, objectName)

	return nil
}
