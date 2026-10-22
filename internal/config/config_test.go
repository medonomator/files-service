package config

import (
	"strings"
	"testing"
	"time"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres:///files")
	t.Setenv("GCS_BUCKET", "bucket")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	settings, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	if settings.HTTPAddr != "127.0.0.1:4010" || settings.MetricsAddr != "" || settings.UploadURLTTL != 15*time.Minute ||
		settings.ResumableThreshold != 16<<20 || settings.DatabaseMaxConns != 10 || settings.RequestTimeout != 20*time.Second {
		t.Fatalf("unexpected defaults %+v", settings)
	}
}

func TestLoadRequiresDatabaseAndBucket(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("GCS_BUCKET", "bucket")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("got %v", err)
	}

	t.Setenv("DATABASE_URL", "postgres:///files")
	t.Setenv("GCS_BUCKET", "")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "GCS_BUCKET") {
		t.Fatalf("got %v", err)
	}
}

func TestLoadRejectsInconsistentSettings(t *testing.T) {
	cases := map[string]map[string]string{
		"UPLOAD_URL_TTL":         {"UPLOAD_URL_TTL": "200h", "PENDING_TTL": "300h"},
		"MAX_DOWNLOAD_URL_TTL":   {"MAX_DOWNLOAD_URL_TTL": "169h"},
		"DOWNLOAD_URL_TTL":       {"DOWNLOAD_URL_TTL": "2h", "MAX_DOWNLOAD_URL_TTL": "1h"},
		"PENDING_TTL":            {"UPLOAD_URL_TTL": "1h", "PENDING_TTL": "30m"},
		"RESUMABLE_THRESHOLD_MB": {"RESUMABLE_THRESHOLD_MB": "4"},
		"JANITOR_INTERVAL":       {"JANITOR_INTERVAL": "soon"},
		"DATABASE_MAX_CONNS":     {"DATABASE_MAX_CONNS": "-1"},
	}

	for variable, env := range cases {
		t.Run(variable, func(t *testing.T) {
			setRequired(t)

			for name, value := range env {
				t.Setenv(name, value)
			}

			if _, err := Load(); err == nil || !strings.Contains(err.Error(), variable) {
				t.Fatalf("expected an error about %s, got %v", variable, err)
			}
		})
	}
}
