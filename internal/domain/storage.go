package domain

import "time"

type SignedRequest struct {
	Method    string
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

type StoredObject struct {
	SizeBytes   int64
	ContentType string
	Generation  int64
}
