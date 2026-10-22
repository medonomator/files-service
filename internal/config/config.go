package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr           string
	MetricsAddr        string
	DatabaseURL        string
	DatabaseMaxConns   int32
	BucketName         string
	UploadURLTTL       time.Duration
	DownloadURLTTL     time.Duration
	MaxDownloadURLTTL  time.Duration
	ResumableThreshold int64
	PendingTTL         time.Duration
	JanitorInterval    time.Duration
	RequestTimeout     time.Duration
	ShutdownTimeout    time.Duration
}

const (
	maxSignedURLTTL       = 7 * 24 * time.Hour
	minResumableThreshold = 5
)

func Load() (Config, error) {
	settings := Config{
		HTTPAddr:    stringValue("HTTP_ADDR", "127.0.0.1:4010"),
		MetricsAddr: os.Getenv("METRICS_ADDR"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		BucketName:  os.Getenv("GCS_BUCKET"),
	}

	if settings.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}

	if settings.BucketName == "" {
		return Config{}, errors.New("GCS_BUCKET is required")
	}

	var err error
	durations := []struct {
		target   *time.Duration
		name     string
		fallback time.Duration
	}{
		{&settings.UploadURLTTL, "UPLOAD_URL_TTL", 15 * time.Minute},
		{&settings.DownloadURLTTL, "DOWNLOAD_URL_TTL", time.Hour},
		{&settings.MaxDownloadURLTTL, "MAX_DOWNLOAD_URL_TTL", maxSignedURLTTL},
		{&settings.PendingTTL, "PENDING_TTL", 24 * time.Hour},
		{&settings.JanitorInterval, "JANITOR_INTERVAL", 10 * time.Minute},
		{&settings.RequestTimeout, "REQUEST_TIMEOUT", 20 * time.Second},
		{&settings.ShutdownTimeout, "SHUTDOWN_TIMEOUT", 15 * time.Second},
	}

	for _, entry := range durations {
		if *entry.target, err = durationValue(entry.name, entry.fallback); err != nil {
			return Config{}, err
		}
	}

	thresholdMB, err := intValue("RESUMABLE_THRESHOLD_MB", 16)
	if err != nil {
		return Config{}, err
	}

	settings.ResumableThreshold = int64(thresholdMB) << 20

	if settings.DatabaseMaxConns, err = connectionLimit("DATABASE_MAX_CONNS", 10); err != nil {
		return Config{}, err
	}

	return settings, settings.validate()
}

func (c Config) validate() error {
	switch {
	case c.UploadURLTTL > maxSignedURLTTL:
		return fmt.Errorf("UPLOAD_URL_TTL must not exceed %s, the limit of signed URLs", maxSignedURLTTL)
	case c.MaxDownloadURLTTL > maxSignedURLTTL:
		return fmt.Errorf("MAX_DOWNLOAD_URL_TTL must not exceed %s, the limit of signed URLs", maxSignedURLTTL)
	case c.DownloadURLTTL > c.MaxDownloadURLTTL:
		return errors.New("DOWNLOAD_URL_TTL must not exceed MAX_DOWNLOAD_URL_TTL")
	case c.PendingTTL <= c.UploadURLTTL:
		return errors.New("PENDING_TTL must be longer than UPLOAD_URL_TTL, or uploads in flight get cleaned up")
	case c.ResumableThreshold < minResumableThreshold<<20:
		return fmt.Errorf("RESUMABLE_THRESHOLD_MB must be at least %d", minResumableThreshold)
	}

	return nil
}

func stringValue(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}

func durationValue(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 10m: %q", name, raw)
	}

	return value, nil
}

func intValue(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > 1<<20 {
		return 0, fmt.Errorf("%s must be a positive integer: %q", name, raw)
	}

	return value, nil
}

func connectionLimit(name string, fallback int32) (int32, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer: %q", name, raw)
	}

	return int32(value), nil
}
