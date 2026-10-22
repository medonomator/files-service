package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/medonomator/files-service/internal/domain"
)

const (
	headerContentType   = "Content-Type"
	headerLengthRange   = "x-goog-content-length-range"
	headerResumable     = "x-goog-resumable"
	resumableStartValue = "start"
)

type GCS struct {
	client *gcs.Client
	bucket *gcs.BucketHandle
}

func NewGCS(ctx context.Context, bucketName string, options ...option.ClientOption) (*GCS, error) {
	client, err := gcs.NewClient(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("create gcs client: %w", err)
	}

	return &GCS{client: client, bucket: client.Bucket(bucketName)}, nil
}

func (s *GCS) Close() error {
	return s.client.Close()
}

func (s *GCS) Ping(ctx context.Context) error {
	_, err := s.bucket.Objects(ctx, &gcs.Query{Prefix: "/"}).Next()
	if err == nil || errors.Is(err, iterator.Done) {
		return nil
	}

	return fmt.Errorf("list bucket: %w", err)
}

func (s *GCS) SignUpload(
	objectName, contentType string,
	sizeBytes int64,
	resumable bool,
	ttl time.Duration,
) (domain.SignedRequest, error) {
	expiresAt := time.Now().Add(ttl)
	lengthRange := fmt.Sprintf("%d,%d", sizeBytes, sizeBytes)

	request := domain.SignedRequest{
		Method:    http.MethodPut,
		ExpiresAt: expiresAt,
		Headers:   map[string]string{headerContentType: contentType, headerLengthRange: lengthRange},
	}
	signedHeaders := []string{headerLengthRange + ":" + lengthRange}

	if resumable {
		request.Method = http.MethodPost
		request.Headers[headerResumable] = resumableStartValue
		signedHeaders = append(signedHeaders, headerResumable+":"+resumableStartValue)
	}

	signedURL, err := s.bucket.SignedURL(objectName, &gcs.SignedURLOptions{
		Scheme:      gcs.SigningSchemeV4,
		Method:      request.Method,
		Expires:     expiresAt,
		ContentType: contentType,
		Headers:     signedHeaders,
	})
	if err != nil {
		return domain.SignedRequest{}, fmt.Errorf("sign upload %s: %w", objectName, err)
	}

	request.URL = signedURL

	return request, nil
}

func (s *GCS) SignDownload(objectName, disposition string, ttl time.Duration) (string, error) {
	signedURL, err := s.bucket.SignedURL(objectName, &gcs.SignedURLOptions{
		Scheme:          gcs.SigningSchemeV4,
		Method:          http.MethodGet,
		Expires:         time.Now().Add(ttl),
		QueryParameters: url.Values{"response-content-disposition": {disposition}},
	})
	if err != nil {
		return "", fmt.Errorf("sign download %s: %w", objectName, err)
	}

	return signedURL, nil
}

func (s *GCS) Stat(ctx context.Context, objectName string) (domain.StoredObject, error) {
	attrs, err := s.bucket.Object(objectName).Attrs(ctx)
	if errors.Is(err, gcs.ErrObjectNotExist) {
		return domain.StoredObject{}, domain.ErrNotFound
	}

	if err != nil {
		return domain.StoredObject{}, fmt.Errorf("stat %s: %w", objectName, err)
	}

	return domain.StoredObject{SizeBytes: attrs.Size, ContentType: attrs.ContentType, Generation: attrs.Generation}, nil
}

func (s *GCS) ReadHead(ctx context.Context, objectName string, generation, length int64) ([]byte, error) {
	reader, err := s.bucket.Object(objectName).Generation(generation).NewRangeReader(ctx, 0, length)
	if errors.Is(err, gcs.ErrObjectNotExist) {
		return nil, domain.ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("open %s#%d: %w", objectName, generation, err)
	}

	defer func() { _ = reader.Close() }()

	head, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read %s#%d: %w", objectName, generation, err)
	}

	return head, nil
}

func (s *GCS) Promote(ctx context.Context, source string, generation int64, target string) error {
	destination := s.bucket.Object(target).If(gcs.Conditions{DoesNotExist: true})
	origin := s.bucket.Object(source).If(gcs.Conditions{GenerationMatch: generation})

	_, err := destination.CopierFrom(origin).Run(ctx)

	switch {
	case err == nil:
	case preconditionFailed(err):
		if _, statErr := s.bucket.Object(target).Attrs(ctx); statErr == nil {
			return domain.ErrAlreadyExists
		}

		return domain.ErrNotFound
	case errors.Is(err, gcs.ErrObjectNotExist):
		return domain.ErrNotFound
	default:
		return fmt.Errorf("copy %s#%d to %s: %w", source, generation, target, err)
	}

	if err := s.Delete(ctx, source); err != nil {
		return fmt.Errorf("drop staging object: %w", err)
	}

	return nil
}

func (s *GCS) Delete(ctx context.Context, objectName string) error {
	err := s.bucket.Object(objectName).Delete(ctx)
	if err == nil || errors.Is(err, gcs.ErrObjectNotExist) {
		return nil
	}

	return fmt.Errorf("delete %s: %w", objectName, err)
}

func preconditionFailed(err error) bool {
	var apiError *googleapi.Error

	return errors.As(err, &apiError) && apiError.Code == http.StatusPreconditionFailed
}
