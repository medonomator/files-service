package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/medonomator/files-service/internal/apikey"
	"github.com/medonomator/files-service/internal/domain"
	"github.com/medonomator/files-service/internal/files"
)

type fakeClients struct {
	key    string
	client domain.Client
	err    error
}

func (f fakeClients) ClientByKeyHash(_ context.Context, hash []byte) (domain.Client, error) {
	if f.err != nil {
		return domain.Client{}, f.err
	}

	if string(hash) != string(apikey.Hash(f.key)) {
		return domain.Client{}, domain.ErrNotFound
	}

	return f.client, nil
}

type fakeFiles struct {
	lastClient  domain.Client
	lastRequest files.CreateUploadRequest
	lastTTL     time.Duration
	err         error
	panics      bool
}

func (f *fakeFiles) CreateUpload(_ context.Context, client domain.Client, request files.CreateUploadRequest) (files.UploadTicket, error) {
	f.lastClient, f.lastRequest = client, request

	return files.UploadTicket{FileID: uuid.New(), UploadMode: files.UploadModeSingle, Method: "PUT"}, f.err
}

func (f *fakeFiles) Complete(_ context.Context, client domain.Client, id uuid.UUID) (files.View, error) {
	f.lastClient = client

	return files.View{ID: id, Status: domain.StatusReady}, f.err
}

func (f *fakeFiles) Get(_ context.Context, client domain.Client, id uuid.UUID, urlTTL time.Duration) (files.View, error) {
	f.lastClient, f.lastTTL = client, urlTTL

	return files.View{ID: id}, f.err
}

func (f *fakeFiles) Delete(_ context.Context, client domain.Client, _ uuid.UUID) error {
	if f.panics {
		panic("boom")
	}

	f.lastClient = client

	return f.err
}

type failingProbe struct{ err error }

func (p failingProbe) Ping(context.Context) error { return p.err }

func newTestRouter(t *testing.T, rate int) (http.Handler, string, *fakeFiles) {
	t.Helper()

	issued, err := apikey.Generate()
	if err != nil {
		t.Fatal(err)
	}

	service := &fakeFiles{}
	router := NewRouter(Dependencies{
		Files:   service,
		Clients: fakeClients{key: issued.Plain, client: domain.Client{ID: 3, Slug: "hs", RateLimitPerMinute: rate}},
		Probes:  map[string]Probe{"ok": failingProbe{}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	return router, issued.Plain, service
}

func send(router http.Handler, method, target, key, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	return recorder
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()

	var body errorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid error body %q", recorder.Body.String())
	}

	return body.Error.Code
}

func TestAuthentication(t *testing.T) {
	router, key, _ := newTestRouter(t, 100)
	other, _ := apikey.Generate()

	for name, candidate := range map[string]string{"missing": "", "garbage": "nope", "unknown": other.Plain} {
		t.Run(name, func(t *testing.T) {
			recorder := send(router, http.MethodGet, "/v1/files/"+uuid.NewString(), candidate, "")
			if recorder.Code != http.StatusUnauthorized || errorCode(t, recorder) != "unauthorized" {
				t.Fatalf("got %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	if recorder := send(router, http.MethodGet, "/v1/files/"+uuid.NewString(), key, ""); recorder.Code != http.StatusOK {
		t.Fatalf("valid key rejected: %d", recorder.Code)
	}
}

func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	router, key, _ := newTestRouter(t, 100)
	request := httptest.NewRequest(http.MethodGet, "/v1/files/"+uuid.NewString(), nil)
	request.Header.Set("Authorization", "bearer "+key)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d", recorder.Code)
	}
}

func TestFailedAuthIsThrottledPerAddress(t *testing.T) {
	router, key, _ := newTestRouter(t, 1000)

	var last int
	for attempt := 0; attempt < 40; attempt++ {
		last = send(router, http.MethodGet, "/v1/files/"+uuid.NewString(), "fsk_wrong", "").Code
	}

	if last != http.StatusTooManyRequests {
		t.Fatalf("brute force must be throttled, got %d", last)
	}

	if recorder := send(router, http.MethodGet, "/v1/files/"+uuid.NewString(), key, ""); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled address must wait even with a valid key, got %d", recorder.Code)
	}
}

func TestRequestIDIsEchoedOrGenerated(t *testing.T) {
	router, _, _ := newTestRouter(t, 10)

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.Header.Set("X-Request-Id", "trace-123")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("X-Request-Id"); got != "trace-123" {
		t.Fatalf("incoming id must be kept, got %q", got)
	}

	request = httptest.NewRequest(http.MethodGet, "/health", nil)
	request.Header.Set("X-Request-Id", "bad id\nwith newline")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if got := recorder.Header().Get("X-Request-Id"); got == "" || strings.ContainsAny(got, " \n") {
		t.Fatalf("invalid id must be replaced, got %q", got)
	}
}

type recordingObserver struct{ routes []string }

func (o *recordingObserver) ObserveRequest(route, method string, status int, _ time.Duration) {
	o.routes = append(o.routes, method+" "+route+" "+http.StatusText(status))
}

func TestObserverSeesRoutePatterns(t *testing.T) {
	issued, _ := apikey.Generate()
	observer := &recordingObserver{}
	router := NewRouter(Dependencies{
		Files:    &fakeFiles{},
		Clients:  fakeClients{key: issued.Plain, client: domain.Client{ID: 1, Slug: "hs", RateLimitPerMinute: 10}},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Observer: observer,
	})

	send(router, http.MethodGet, "/v1/files/"+uuid.NewString(), issued.Plain, "")

	if len(observer.routes) != 1 || observer.routes[0] != "GET /v1/files/{fileId} OK" {
		t.Fatalf("unexpected observations %v", observer.routes)
	}
}

func TestPanicsBecomeInternalErrors(t *testing.T) {
	router, key, service := newTestRouter(t, 10)
	service.panics = true

	recorder := send(router, http.MethodDelete, "/v1/files/"+uuid.NewString(), key, "")
	if recorder.Code != http.StatusInternalServerError || errorCode(t, recorder) != "internal" {
		t.Fatalf("got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestLookupFailureIsInternalError(t *testing.T) {
	issued, _ := apikey.Generate()
	router := NewRouter(Dependencies{
		Files:   &fakeFiles{},
		Clients: fakeClients{key: issued.Plain, err: errors.New("db down")},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if recorder := send(router, http.MethodGet, "/v1/files/"+uuid.NewString(), issued.Plain, ""); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", recorder.Code)
	}
}

func TestCreateUpload(t *testing.T) {
	router, key, service := newTestRouter(t, 100)

	recorder := send(router, http.MethodPost, "/v1/uploads", key,
		`{"fileName":"a.mp4","contentType":"video/mp4","sizeBytes":42,"folder":"lessons","ownerRef":"group:maid"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("got %d %s", recorder.Code, recorder.Body.String())
	}

	if service.lastClient.Slug != "hs" || service.lastRequest.SizeBytes != 42 || service.lastRequest.OwnerRef != "group:maid" {
		t.Fatalf("request not passed through: %+v %+v", service.lastClient, service.lastRequest)
	}

	for body, status := range map[string]int{
		`{`:                 http.StatusBadRequest,
		`{"unknown":1}`:     http.StatusBadRequest,
		`{"sizeBytes":"1"}`: http.StatusBadRequest,
		`{} {}`:             http.StatusBadRequest,
		strings.Repeat(" ", maxRequestBodyBytes+1) + "{}": http.StatusRequestEntityTooLarge,
	} {
		if recorder := send(router, http.MethodPost, "/v1/uploads", key, body); recorder.Code != status {
			t.Fatalf("body %.20q: got %d, want %d", body, recorder.Code, status)
		}
	}
}

func TestDomainErrorsMapToStatuses(t *testing.T) {
	router, key, service := newTestRouter(t, 100)

	cases := map[error]int{
		domain.TooLarge("x"):      http.StatusRequestEntityTooLarge,
		domain.QuotaExceeded("x"): http.StatusForbidden,
		domain.Unsupported("x"):   http.StatusUnsupportedMediaType,
		domain.Conflict("x"):      http.StatusConflict,
		errors.New("boom"):        http.StatusInternalServerError,
	}

	for err, status := range cases {
		service.err = err
		if recorder := send(router, http.MethodPost, "/v1/uploads", key, `{}`); recorder.Code != status {
			t.Fatalf("%v: got %d, want %d", err, recorder.Code, status)
		}
	}
}

func TestFileRoutes(t *testing.T) {
	router, key, service := newTestRouter(t, 100)
	id := uuid.NewString()

	if recorder := send(router, http.MethodPost, "/v1/files/"+id+"/complete", key, ""); recorder.Code != http.StatusOK {
		t.Fatalf("complete: %d", recorder.Code)
	}

	if recorder := send(router, http.MethodGet, "/v1/files/"+id+"?urlTtl=600", key, ""); recorder.Code != http.StatusOK || service.lastTTL != 10*time.Minute {
		t.Fatalf("get: %d ttl %s", recorder.Code, service.lastTTL)
	}

	for _, ttl := range []string{"-1", "0", "abc", "604801", "9223372036854775807"} {
		if recorder := send(router, http.MethodGet, "/v1/files/"+id+"?urlTtl="+ttl, key, ""); recorder.Code != http.StatusBadRequest {
			t.Fatalf("ttl %s: %d", ttl, recorder.Code)
		}
	}

	if recorder := send(router, http.MethodDelete, "/v1/files/"+id, key, ""); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", recorder.Code)
	}

	if recorder := send(router, http.MethodGet, "/v1/files/not-a-uuid", key, ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("bad id: %d", recorder.Code)
	}
}

func TestRateLimitPerClient(t *testing.T) {
	router, key, _ := newTestRouter(t, 3)
	target := "/v1/files/" + uuid.NewString()

	for attempt := 0; attempt < 3; attempt++ {
		if recorder := send(router, http.MethodGet, target, key, ""); recorder.Code != http.StatusOK {
			t.Fatalf("attempt %d: %d", attempt, recorder.Code)
		}
	}

	recorder := send(router, http.MethodGet, target, key, "")
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 429, got %d", recorder.Code)
	}
}

func TestProbes(t *testing.T) {
	router, _, _ := newTestRouter(t, 10)

	if recorder := send(router, http.MethodGet, "/health", "", ""); recorder.Code != http.StatusOK {
		t.Fatalf("health: %d", recorder.Code)
	}

	if recorder := send(router, http.MethodGet, "/ready", "", ""); recorder.Code != http.StatusOK {
		t.Fatalf("ready: %d", recorder.Code)
	}

	broken := NewRouter(Dependencies{
		Files:   &fakeFiles{},
		Clients: fakeClients{},
		Probes:  map[string]Probe{"postgres": failingProbe{err: errors.New("down")}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if recorder := send(broken, http.MethodGet, "/ready", "", ""); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready with broken probe: %d", recorder.Code)
	}
}
