package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"

	"github.com/medonomator/files-service/internal/apikey"
	"github.com/medonomator/files-service/internal/domain"
)

const (
	requestIDHeader     = "X-Request-Id"
	authFailuresPerMin  = 30
	readinessCacheTTL   = 10 * time.Second
	readinessTimeout    = 3 * time.Second
	maxTrackedAddresses = 10_000
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type clientKey struct{}

type requestIDKey struct{}

func clientFrom(ctx context.Context) domain.Client {
	client, _ := ctx.Value(clientKey{}).(domain.Client)

	return client
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)

	return id
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}

		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func newRequestID() string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)

	return hex.EncodeToString(raw)
}

func timeout(limit time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), limit)
			defer cancel()

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func authenticate(clients ClientLookup, failures *addressLimiter, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			address := remoteAddress(r)
			if !failures.permits(address) {
				tooManyRequests(w, failures.retryAfter(address))
				return
			}

			key, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok || !apikey.WellFormed(key) {
				failures.record(address)
				unauthorized(w)

				return
			}

			client, err := clients.ClientByKeyHash(r.Context(), apikey.Hash(key))
			if errors.Is(err, domain.ErrNotFound) {
				failures.record(address)
				unauthorized(w)

				return
			}

			if err != nil {
				writeError(w, r, logger, err)
				return
			}

			if entry, ok := r.Context().Value(logEntryKey{}).(*logEntry); ok {
				entry.client = client.Slug
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, client)))
		})
	}
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}

	return strings.TrimSpace(token), true
}

func remoteAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeJSON(w, http.StatusUnauthorized, errorBody{Error: errorDetail{Code: "unauthorized", Message: "Invalid API key"}})
}

func tooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(max(retryAfter, time.Second).Seconds()))))
	writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Code: "rate_limited", Message: "Too many requests"}})
}

type clientLimiter struct {
	mu      sync.Mutex
	buckets map[int64]*rate.Limiter
}

func newClientLimiter() *clientLimiter {
	return &clientLimiter{buckets: map[int64]*rate.Limiter{}}
}

func (l *clientLimiter) reserve(client domain.Client) (bool, time.Duration) {
	perSecond := rate.Limit(float64(client.RateLimitPerMinute) / 60)

	l.mu.Lock()
	bucket, ok := l.buckets[client.ID]
	if !ok || bucket.Limit() != perSecond {
		bucket = rate.NewLimiter(perSecond, client.RateLimitPerMinute)
		l.buckets[client.ID] = bucket
	}
	l.mu.Unlock()

	now := time.Now()
	if bucket.AllowN(now, 1) {
		return true, 0
	}

	reservation := bucket.ReserveN(now, 1)
	delay := reservation.DelayFrom(now)
	reservation.CancelAt(now)

	return false, delay
}

func limitRate(limits *clientLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, retryAfter := limits.reserve(clientFrom(r.Context())); !ok {
				tooManyRequests(w, retryAfter)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

type addressLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

func newAddressLimiter() *addressLimiter {
	return &addressLimiter{buckets: map[string]*rate.Limiter{}}
}

func (l *addressLimiter) bucket(address string, create bool) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, ok := l.buckets[address]
	if !ok && create {
		if len(l.buckets) >= maxTrackedAddresses {
			clear(l.buckets)
		}

		bucket = rate.NewLimiter(rate.Limit(authFailuresPerMin/60.0), authFailuresPerMin)
		l.buckets[address] = bucket
	}

	return bucket
}

func (l *addressLimiter) permits(address string) bool {
	bucket := l.bucket(address, false)

	return bucket == nil || bucket.Tokens() >= 1
}

func (l *addressLimiter) record(address string) {
	l.bucket(address, true).Allow()
}

func (l *addressLimiter) retryAfter(address string) time.Duration {
	bucket := l.bucket(address, false)
	if bucket == nil {
		return 0
	}

	return time.Duration((1 - bucket.Tokens()) / float64(bucket.Limit()) * float64(time.Second))
}

type logEntryKey struct{}

type logEntry struct {
	client string
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = status, true
	}

	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}

	written, err := r.ResponseWriter.Write(body)
	r.bytes += written

	return written, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func accessLog(logger *slog.Logger, observer RequestObserver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			entry := &logEntry{}
			recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), logEntryKey{}, entry)))

			route := "unmatched"
			if routeContext := chi.RouteContext(r.Context()); routeContext != nil && routeContext.RoutePattern() != "" {
				route = routeContext.RoutePattern()
			}

			duration := time.Since(started)
			observer.ObserveRequest(route, r.Method, recorder.status, duration)

			if route == "/health" {
				return
			}

			logger.InfoContext(r.Context(), "request",
				"requestId", RequestID(r.Context()),
				"method", r.Method,
				"route", route,
				"status", recorder.status,
				"bytes", recorder.bytes,
				"durationMs", duration.Milliseconds(),
				"client", entry.client,
				"remote", remoteAddress(r),
			)
		})
	}
}

func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}

				if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(recovered)
				}

				logger.ErrorContext(r.Context(), "panic",
					"requestId", RequestID(r.Context()), "value", recovered, "stack", string(debug.Stack()),
				)
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{Code: "internal", Message: "Internal error"}})
			}()

			next.ServeHTTP(w, r)
		})
	}
}

type readinessCache struct {
	probes  map[string]Probe
	logger  *slog.Logger
	mu      sync.Mutex
	checked time.Time
	ready   bool
}

func (c *readinessCache) handle(w http.ResponseWriter, r *http.Request) {
	if c.check(r.Context()) {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
}

func (c *readinessCache) check(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.checked.IsZero() && time.Since(c.checked) < readinessCacheTTL {
		return c.ready
	}

	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()

	c.ready = true

	for name, probe := range c.probes {
		if err := probe.Ping(ctx); err != nil {
			c.logger.WarnContext(ctx, "readiness probe failed", "probe", name, "error", err)
			c.ready = false

			break
		}
	}

	c.checked = time.Now()

	return c.ready
}
