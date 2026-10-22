package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/medonomator/files-service/internal/domain"
	"github.com/medonomator/files-service/internal/files"
)

const defaultRequestTimeout = 20 * time.Second

type FileService interface {
	CreateUpload(ctx context.Context, client domain.Client, request files.CreateUploadRequest) (files.UploadTicket, error)
	Complete(ctx context.Context, client domain.Client, id uuid.UUID) (files.View, error)
	Get(ctx context.Context, client domain.Client, id uuid.UUID, urlTTL time.Duration) (files.View, error)
	Delete(ctx context.Context, client domain.Client, id uuid.UUID) error
}

type ClientLookup interface {
	ClientByKeyHash(ctx context.Context, hash []byte) (domain.Client, error)
}

type Probe interface {
	Ping(ctx context.Context) error
}

type RequestObserver interface {
	ObserveRequest(route, method string, status int, duration time.Duration)
}

type noObserver struct{}

func (noObserver) ObserveRequest(string, string, int, time.Duration) {}

type Dependencies struct {
	Files          FileService
	Clients        ClientLookup
	Probes         map[string]Probe
	Logger         *slog.Logger
	Observer       RequestObserver
	RequestTimeout time.Duration
	Version        string
}

func NewRouter(dependencies Dependencies) http.Handler {
	if dependencies.Observer == nil {
		dependencies.Observer = noObserver{}
	}

	if dependencies.RequestTimeout == 0 {
		dependencies.RequestTimeout = defaultRequestTimeout
	}

	handler := &handler{files: dependencies.Files, logger: dependencies.Logger}
	ready := &readinessCache{probes: dependencies.Probes, logger: dependencies.Logger}

	router := chi.NewRouter()
	router.Use(requestID)
	router.Use(accessLog(dependencies.Logger, dependencies.Observer))
	router.Use(recoverer(dependencies.Logger))
	router.Use(timeout(dependencies.RequestTimeout))

	router.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	router.Get("/ready", ready.handle)
	router.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": dependencies.Version})
	})

	router.Route("/v1", func(scoped chi.Router) {
		scoped.Use(authenticate(dependencies.Clients, newAddressLimiter(), dependencies.Logger))
		scoped.Use(limitRate(newClientLimiter()))

		scoped.Post("/uploads", handler.createUpload)
		scoped.Post("/files/{fileId}/complete", handler.complete)
		scoped.Get("/files/{fileId}", handler.get)
		scoped.Delete("/files/{fileId}", handler.remove)
	})

	router.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: errorDetail{Code: "not_found", Message: "Route not found"}})
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: errorDetail{Code: "method_not_allowed", Message: "Method not allowed"}})
	})

	return router
}
