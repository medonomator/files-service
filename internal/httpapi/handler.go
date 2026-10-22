package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/medonomator/files-service/internal/domain"
	"github.com/medonomator/files-service/internal/files"
)

const (
	maxRequestBodyBytes = 16 << 10
	maxURLTTLSeconds    = 7 * 24 * 60 * 60
)

type handler struct {
	files  FileService
	logger *slog.Logger
}

type createUploadBody struct {
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Folder      string `json:"folder"`
	OwnerRef    string `json:"ownerRef"`
}

func (h *handler) createUpload(w http.ResponseWriter, r *http.Request) {
	var body createUploadBody
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, h.logger, err)
		return
	}

	ticket, err := h.files.CreateUpload(r.Context(), clientFrom(r.Context()), files.CreateUploadRequest{
		FileName:    body.FileName,
		ContentType: body.ContentType,
		SizeBytes:   body.SizeBytes,
		Folder:      body.Folder,
		OwnerRef:    body.OwnerRef,
	})
	if err != nil {
		writeError(w, r, h.logger, err)
		return
	}

	writeJSON(w, http.StatusCreated, ticket)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	decoder.DisallowUnknownFields()

	err := decoder.Decode(target)
	if err == nil && decoder.Decode(&struct{}{}) != io.EOF {
		return domain.Invalid("Request body must contain a single JSON object")
	}

	var tooLarge *http.MaxBytesError
	var unknownField *json.UnmarshalTypeError

	switch {
	case err == nil:
		return nil
	case errors.As(err, &tooLarge):
		return domain.TooLarge("Request body exceeds %d bytes", maxRequestBodyBytes)
	case errors.As(err, &unknownField):
		return domain.Invalid("Field %q has the wrong type", unknownField.Field)
	default:
		return domain.Invalid("Request body must be a valid JSON object with known fields")
	}
}

func (h *handler) complete(w http.ResponseWriter, r *http.Request) {
	id, ok := fileID(w, r, h.logger)
	if !ok {
		return
	}

	view, err := h.files.Complete(r.Context(), clientFrom(r.Context()), id)
	if err != nil {
		writeError(w, r, h.logger, err)
		return
	}

	writeJSON(w, http.StatusOK, view)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := fileID(w, r, h.logger)
	if !ok {
		return
	}

	var urlTTL time.Duration

	if raw := r.URL.Query().Get("urlTtl"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > maxURLTTLSeconds {
			writeError(w, r, h.logger, domain.Invalid("urlTtl must be between 1 and %d seconds", maxURLTTLSeconds))
			return
		}

		urlTTL = time.Duration(seconds) * time.Second
	}

	view, err := h.files.Get(r.Context(), clientFrom(r.Context()), id, urlTTL)
	if err != nil {
		writeError(w, r, h.logger, err)
		return
	}

	writeJSON(w, http.StatusOK, view)
}

func (h *handler) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := fileID(w, r, h.logger)
	if !ok {
		return
	}

	if err := h.files.Delete(r.Context(), clientFrom(r.Context()), id); err != nil {
		writeError(w, r, h.logger, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func fileID(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "fileId"))
	if err != nil {
		writeError(w, r, logger, domain.NotFound("File not found"))
		return uuid.UUID{}, false
	}

	return id, true
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func statusFor(kind domain.ErrorKind) (int, string) {
	switch kind {
	case domain.KindInvalid:
		return http.StatusBadRequest, "invalid_request"
	case domain.KindNotFound:
		return http.StatusNotFound, "not_found"
	case domain.KindConflict:
		return http.StatusConflict, "conflict"
	case domain.KindTooLarge:
		return http.StatusRequestEntityTooLarge, "too_large"
	case domain.KindQuotaExceeded:
		return http.StatusForbidden, "quota_exceeded"
	case domain.KindUnsupported:
		return http.StatusUnsupportedMediaType, "unsupported_type"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

func writeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	var known *domain.Error
	if errors.As(err, &known) {
		status, code := statusFor(known.Kind)
		writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: known.Message}})

		return
	}

	logger.ErrorContext(r.Context(), "request failed", "requestId", RequestID(r.Context()), "error", err, "path", r.URL.Path)
	writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: "internal", Message: "Internal error"}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
