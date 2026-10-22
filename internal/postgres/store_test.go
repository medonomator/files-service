package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/medonomator/files-service/internal/apikey"
	"github.com/medonomator/files-service/internal/domain"
	"github.com/medonomator/files-service/migrations"
)

func openStore(t *testing.T) *Store {
	t.Helper()

	databaseURL := os.Getenv("FILES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FILES_TEST_DATABASE_URL is not set")
	}

	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())

	admin, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})

	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}

	scoped, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	query := scoped.Query()
	query.Set("search_path", schema)
	scoped.RawQuery = query.Encode()

	db, err := sql.Open("pgx", scoped.String())
	if err != nil {
		t.Fatal(err)
	}

	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}

	if err := goose.Up(db, "."); err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(context.Background(), scoped.String())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(pool.Close)

	return New(pool)
}

func TestStore(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	client, err := store.CreateClient(ctx, domain.Client{
		Slug: "hs", Name: "Home Sharing", AllowedTypes: []string{"video/*"},
		MaxFileBytes: 1 << 30, QuotaBytes: 10 << 30, RateLimitPerMinute: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.CreateClient(ctx, domain.Client{Slug: "hs", Name: "dup", AllowedTypes: []string{"*"}, MaxFileBytes: 1, QuotaBytes: 1, RateLimitPerMinute: 1}); err == nil {
		t.Fatal("duplicate slug must fail")
	}

	issued, _ := apikey.Generate()
	if err := store.AddKey(ctx, client.ID, issued.Hash, issued.Display); err != nil {
		t.Fatal(err)
	}

	found, err := store.ClientByKeyHash(ctx, issued.Hash)
	if err != nil || found.ID != client.ID || found.AllowedTypes[0] != "video/*" {
		t.Fatalf("lookup by key: %+v %v", found, err)
	}

	t.Run("disabled client is not authenticated", func(t *testing.T) {
		if err := store.SetClientDisabled(ctx, "hs", true); err != nil {
			t.Fatal(err)
		}

		if _, err := store.ClientByKeyHash(ctx, issued.Hash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("expected not found, got %v", err)
		}

		if err := store.SetClientDisabled(ctx, "hs", false); err != nil {
			t.Fatal(err)
		}

		if err := store.SetClientDisabled(ctx, "nobody", true); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("unknown slug: %v", err)
		}
	})

	t.Run("file lifecycle and quota accounting", func(t *testing.T) {
		pendingID, readyID, failedID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		owner := "group:maid"

		for _, id := range []uuid.UUID{pendingID, readyID, failedID} {
			if _, err := store.CreateFile(ctx, domain.File{
				ID: id, ClientID: client.ID, Status: domain.StatusPending, Folder: "lessons", OwnerRef: &owner,
				FileName: "a.mp4", ObjectName: "hs/lessons/" + id.String() + ".mp4", ContentType: "video/mp4", DeclaredSizeBytes: 100,
			}); err != nil {
				t.Fatal(err)
			}
		}

		ready, err := store.MarkReady(ctx, readyID, 90)
		if err != nil || ready.Status != domain.StatusReady || *ready.SizeBytes != 90 || ready.ReadyAt == nil {
			t.Fatalf("mark ready: %+v %v", ready, err)
		}

		if _, err := store.MarkReady(ctx, readyID, 90); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("second mark ready must not match: %v", err)
		}

		if claimed, err := store.MarkFailed(ctx, failedID); err != nil || !claimed {
			t.Fatalf("mark failed: %v %v", claimed, err)
		}

		if claimed, err := store.MarkFailed(ctx, failedID); err != nil || claimed {
			t.Fatalf("second mark failed must not claim: %v %v", claimed, err)
		}

		if reserved := reservedBytes(t, store, client.ID); reserved != 200 {
			t.Fatalf("reserved = %d, want 200 (ready and pending keep their declared size)", reserved)
		}

		if _, err := store.FileByID(ctx, client.ID+1, readyID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("file must be scoped to client: %v", err)
		}

		if stale, err := store.UnfinishedOlderThan(ctx, time.Hour, 10); err != nil || len(stale) != 0 {
			t.Fatalf("fresh uploads must not be stale: %d %v", len(stale), err)
		}

		if _, err := store.pool.Exec(ctx, `UPDATE files SET created_at = now() - interval '2 hours'`); err != nil {
			t.Fatal(err)
		}

		stale, err := store.UnfinishedOlderThan(ctx, time.Hour, 10)
		if err != nil || len(stale) != 2 {
			t.Fatalf("unfinished: %d %v", len(stale), err)
		}

		if err := store.DeleteFailed(ctx, readyID); err != nil {
			t.Fatal(err)
		}

		if _, err := store.FileByID(ctx, client.ID, readyID); err != nil {
			t.Fatalf("DeleteFailed must not touch ready files: %v", err)
		}

		if err := store.DeleteFailed(ctx, failedID); err != nil {
			t.Fatal(err)
		}

		if err := store.DeleteFile(ctx, pendingID); err != nil {
			t.Fatal(err)
		}

		summaries, err := store.ListClients(ctx)
		if err != nil || len(summaries) != 1 || summaries[0].FileCount != 1 || summaries[0].UsedBytes != 100 || len(summaries[0].ActiveKeys) != 1 {
			t.Fatalf("summary: %+v %v", summaries, err)
		}
	})

	t.Run("revoked key stops working", func(t *testing.T) {
		if err := store.RevokeKey(ctx, issued.Display); err != nil {
			t.Fatal(err)
		}

		if _, err := store.ClientByKeyHash(ctx, issued.Hash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("expected not found, got %v", err)
		}

		if err := store.RevokeKey(ctx, issued.Display); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("double revoke: %v", err)
		}
	})
}

func reservedBytes(t *testing.T, store *Store, clientID int64) int64 {
	t.Helper()

	var reserved int64
	if err := store.pool.QueryRow(context.Background(), `SELECT reserved_bytes FROM clients WHERE id = $1`, clientID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}

	return reserved
}

func TestQuotaHoldsUnderConcurrency(t *testing.T) {
	store := openStore(t)
	ctx := context.Background()

	client, err := store.CreateClient(ctx, domain.Client{
		Slug: "racer", Name: "Racer", AllowedTypes: []string{"*"},
		MaxFileBytes: 100, QuotaBytes: 1000, RateLimitPerMinute: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 50

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
	)

	for range attempts {
		wg.Add(1)

		go func() {
			defer wg.Done()

			id := uuid.Must(uuid.NewV7())
			_, err := store.CreateFile(ctx, domain.File{
				ID: id, ClientID: client.ID, Status: domain.StatusPending, Folder: "files",
				FileName: "a.bin", ObjectName: "racer/files/" + id.String() + ".bin", ContentType: "application/pdf", DeclaredSizeBytes: 100,
			})

			switch {
			case err == nil:
				mu.Lock()
				accepted++
				mu.Unlock()
			case !errors.Is(err, domain.ErrQuotaExceeded):
				t.Error(err)
			}
		}()
	}

	wg.Wait()

	if accepted != 10 {
		t.Fatalf("accepted %d uploads of 100 bytes into a 1000 byte quota", accepted)
	}

	if reserved := reservedBytes(t, store, client.ID); reserved != 1000 {
		t.Fatalf("reserved = %d, want 1000", reserved)
	}
}
