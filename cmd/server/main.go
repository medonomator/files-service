package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/medonomator/files-service/internal/config"
	"github.com/medonomator/files-service/internal/files"
	"github.com/medonomator/files-service/internal/httpapi"
	"github.com/medonomator/files-service/internal/janitor"
	"github.com/medonomator/files-service/internal/metrics"
	"github.com/medonomator/files-service/internal/postgres"
	"github.com/medonomator/files-service/internal/storage"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("files-service stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	settings, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx, settings)
	if err != nil {
		return err
	}

	defer pool.Close()

	bucket, err := storage.NewGCS(ctx, settings.BucketName)
	if err != nil {
		return err
	}

	defer bucket.Close()

	registry := metrics.New(version)
	registry.WatchPool(pool)

	store := postgres.New(pool)
	service := files.NewService(store, bucket, files.Settings{
		UploadURLTTL:       settings.UploadURLTTL,
		DownloadURLTTL:     settings.DownloadURLTTL,
		MaxDownloadURLTTL:  settings.MaxDownloadURLTTL,
		ResumableThreshold: settings.ResumableThreshold,
	}).WithMetrics(registry)

	servers := []*http.Server{newServer(settings.HTTPAddr, httpapi.NewRouter(httpapi.Dependencies{
		Files:          service,
		Clients:        store,
		Probes:         map[string]httpapi.Probe{"postgres": store, "gcs": bucket},
		Logger:         logger,
		Observer:       registry,
		RequestTimeout: settings.RequestTimeout,
		Version:        version,
	}))}

	if settings.MetricsAddr != "" {
		servers = append(servers, newServer(settings.MetricsAddr, registry.Handler()))
	}

	listeners := make([]net.Listener, 0, len(servers))
	var listenConfig net.ListenConfig

	for _, server := range servers {
		listener, err := listenConfig.Listen(ctx, "tcp", server.Addr)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}

			return fmt.Errorf("listen on %s: %w", server.Addr, err)
		}

		listeners = append(listeners, listener)
	}

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		janitor.New(store, bucket, settings.PendingTTL, logger).OnSweep(registry.JanitorSwept).Run(groupCtx, settings.JanitorInterval)

		return nil
	})

	for index, server := range servers {
		logger.Info("files-service listening", "addr", server.Addr, "version", version)

		group.Go(func() error {
			if err := server.Serve(listeners[index]); !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serve %s: %w", server.Addr, err)
			}

			return nil
		})
	}

	group.Go(func() error {
		<-groupCtx.Done()
		stop()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(groupCtx), settings.ShutdownTimeout)
		defer cancel()

		var shutdownErr error
		for _, server := range servers {
			shutdownErr = errors.Join(shutdownErr, server.Shutdown(shutdownCtx))
		}

		return shutdownErr
	})

	return group.Wait()
}

func openPool(ctx context.Context, settings config.Config) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(settings.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	poolConfig.MaxConns = settings.DatabaseMaxConns
	poolConfig.ConnConfig.RuntimeParams["application_name"] = "files-service"
	poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = "10s"

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return pool, nil
}

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}
