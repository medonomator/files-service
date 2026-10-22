# files-service

[![ci](https://github.com/medonomator/files-service/actions/workflows/ci.yml/badge.svg)](https://github.com/medonomator/files-service/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/medonomator/files-service.svg)](https://pkg.go.dev/github.com/medonomator/files-service)
[![Go Report Card](https://goreportcard.com/badge/github.com/medonomator/files-service)](https://goreportcard.com/report/github.com/medonomator/files-service)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A small multi-tenant file upload service in Go. Backends of other products authenticate with their own API key and get short-lived signed URLs. Browsers and mobile apps then upload straight to Google Cloud Storage, so file bytes never pass through the service. In production a single instance handles gigabyte videos in about 36 MB of RAM.

It runs in production behind [Home Sharing](https://home-sharing24.ru), an apart-hotel platform, where it stores training videos, guest video reviews and staff documents.

## Highlights

- **Direct-to-storage uploads.** V4 signed URLs pin the content type and the exact byte size (`x-goog-content-length-range`), so the bucket rejects anything that wasn't declared. Large files switch to GCS resumable sessions with chunked `PUT`s.
- **Verify, then publish.** Uploads land under a staging prefix. `complete` reads the first bytes of that exact object generation, checks the real file signature (magic bytes) against the declared type and only then copies it to its final name with generation preconditions. Swapping the content after the check, or a PNG renamed to `.mp4`, never reaches the final path.
- **Race-free quotas.** Storage is reserved in the same SQL statement that inserts the upload, so parallel requests can't overshoot a client's quota. Rejected, expired and deleted uploads release their reservation.
- **Multi-tenancy.** Every query is scoped to the calling client. Another tenant's file is indistinguishable from a missing one (404).
- **Per-client policy.** Allowed MIME patterns, max file size, storage quota and a request-per-minute limit are stored per client and enforced on every call.
- **API keys done right.** Keys are `fsk_` plus 32 random bytes. Only the SHA-256 is stored, comparison is constant time, a client can hold several keys for rotation and failed attempts are throttled per address.
- **Safe catalog.** HTML, SVG and executables are never accepted. File names are sanitized, keep the extension of their real type and are served with RFC 6266 `Content-Disposition`.
- **Self-cleaning.** A janitor claims abandoned uploads atomically before it removes them, so it can't race a late `complete`.
- **Operable.** Structured JSON logs with request ids, Prometheus metrics on a separate listener, cached `/ready` probe, request timeouts, graceful shutdown, a hardened systemd unit and a distroless image.

## How an upload works

```text
  browser / app  ======== signed PUT or POST ========  GCS bucket
        |                                                  |
        | asks to upload                                   | stat, read head,
        |                                                  | promote
  client backend  ======== Bearer fsk_... ==========  files-service  ======  Postgres
```

1. The client backend calls `POST /v1/uploads` with `Authorization: Bearer fsk_...` and gets `uploadUrl`, `method` and `headers`.
2. The browser sends the file to `uploadUrl` with exactly those headers.
   - `uploadMode: single` is one `PUT`.
   - `uploadMode: resumable` starts with a `POST`, takes the session URL from the `Location` header and then sends `PUT` chunks of `chunkSizeBytes` with `Content-Range`.
3. The client backend calls `POST /v1/files/{id}/complete`. The service checks size, type and signature of the staged object and moves it to its final name. On success the file becomes `ready` and the response carries a `downloadUrl`. The call is idempotent and safe to retry.
4. `GET /v1/files/{id}?urlTtl=3600` returns a fresh signed download link (1 hour by default, 7 days at most).
5. `DELETE /v1/files/{id}` removes the file.

The API key only ever lives on the client backend. The browser only sees signed links that expire in minutes.

## API

The full contract is in [`api/openapi.yaml`](api/openapi.yaml).

`POST /v1/uploads`

```json
{"fileName": "Lesson 1.mp4", "contentType": "video/mp4", "sizeBytes": 73400320, "folder": "lessons/maid", "ownerRef": "group:maid"}
```

`folder` is optional (default `files`, up to 4 segments of `a-z0-9_-`). `ownerRef` is a free client-side label of up to 128 characters.

Response `201`:

```json
{
  "fileId": "0192f0c4-5d6e-7a00-8000-000000000001",
  "uploadMode": "resumable",
  "method": "POST",
  "uploadUrl": "https://storage.googleapis.com/...",
  "headers": {"Content-Type": "video/mp4", "x-goog-content-length-range": "73400320,73400320", "x-goog-resumable": "start"},
  "chunkSizeBytes": 8388608,
  "expiresAt": "2026-10-22T20:15:00Z"
}
```

Errors always have the same shape, `{"error": {"code": "...", "message": "..."}}`, and every response carries an `X-Request-Id` that also appears in the logs.

| HTTP | code | when |
|---|---|---|
| 400 | `invalid_request` | malformed body, or the content doesn't match the declaration |
| 401 | `unauthorized` | missing, unknown or revoked key, or a disabled client |
| 403 | `quota_exceeded` | the client's storage quota is used up |
| 404 | `not_found` | no such file for this client |
| 409 | `conflict` | `complete` before the upload finished, or after it was rejected |
| 413 | `too_large` | above the client's `max_file_bytes`, or a request body over 16 KB |
| 415 | `unsupported_type` | type not in the catalog or not allowed for the client |
| 429 | `rate_limited` | over the client's per-minute limit, or too many bad keys from one address (`Retry-After` is set) |

Accepted types are images (jpeg, png, gif, webp, heic/heif, avif), video (mp4, mov, webm), audio (mp3, m4a, ogg, wav), documents (pdf, zip, doc/xls/ppt, docx/xlsx/pptx) and text (txt, csv, md, json).

## Project layout

```text
cmd/
  server/      HTTP server, janitor, graceful shutdown
  migrate/     goose migrations runner
  clients/     admin CLI for tenants and keys
internal/
  domain/      entities, validation, content catalog and signature checks
  files/       use cases: create upload, complete, get, delete
  httpapi/     chi router, auth, rate limits, request ids, logging, error mapping
  postgres/    pgx store
  storage/     GCS adapter (signed URLs, resumable sessions, object reads)
  janitor/     cleanup of abandoned uploads
  metrics/     Prometheus collectors
  apikey/      key generation and hashing
api/           OpenAPI 3.1 contract
migrations/    embedded SQL migrations
deploy/        systemd unit
```

The `files` package depends only on interfaces declared next to it, so the use cases are tested with in-memory fakes and the storage and database adapters are tested separately.

## Managing clients

```bash
bin/clients create -slug hs -name "Home Sharing" -types "video/*,image/*,application/pdf" -max-file-mb 2048 -quota-gb 100 -rate 120
bin/clients key -slug hs
bin/clients revoke -key fsk_XXXXXXXX
bin/clients disable -slug hs
bin/clients list
```

A key is printed once, when it's issued.

## Running locally

Requirements are Go 1.26+ (the module pins the 1.27 toolchain), Docker, and a GCS bucket with a service account that has `roles/storage.objectAdmin` on it.

```bash
make db                 # Postgres in Docker on :5471
cp .env.example .env    # set GCS_BUCKET and GOOGLE_APPLICATION_CREDENTIALS
make migrate
make run
```

```bash
make test               # unit tests with the race detector
make test-integration   # adds the Postgres store tests
make lint
make vuln
```

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | required | Postgres connection string |
| `GCS_BUCKET` | required | target bucket |
| `GOOGLE_APPLICATION_CREDENTIALS` | ADC | service account key file |
| `HTTP_ADDR` | `127.0.0.1:4010` | listen address |
| `METRICS_ADDR` | empty | Prometheus listener, disabled when empty |
| `DATABASE_MAX_CONNS` | `10` | Postgres pool size |
| `REQUEST_TIMEOUT` | `20s` | per-request deadline |
| `UPLOAD_URL_TTL` | `15m` | lifetime of signed upload URLs |
| `DOWNLOAD_URL_TTL` | `1h` | default lifetime of download URLs |
| `MAX_DOWNLOAD_URL_TTL` | `168h` | upper bound for `urlTtl` |
| `RESUMABLE_THRESHOLD_MB` | `16` | files from this size use resumable uploads, at least 5 |
| `PENDING_TTL` | `24h` | when an unfinished upload is cleaned up, must exceed `UPLOAD_URL_TTL` |
| `JANITOR_INTERVAL` | `10m` | how often the janitor runs |
| `SHUTDOWN_TIMEOUT` | `15s` | graceful shutdown budget |

## Bucket setup

The bucket should have uniform bucket-level access and public access prevention enabled, so objects are only reachable through signed URLs. CORS has to allow `PUT, POST, GET, HEAD` and expose the `Location` header for resumable uploads. Access is controlled by the signature, not by origin.

Add a lifecycle rule that deletes objects under `incoming/` after a few days. It's a safety net for staging objects whose rows were already cleaned up.

```bash
cat > lifecycle.json <<'JSON'
{"rule": [{"action": {"type": "Delete"}, "condition": {"age": 8, "matchesPrefix": ["incoming/"]}}]}
JSON
gcloud storage buckets update gs://your-bucket --lifecycle-file=lifecycle.json
```

## Observability

With `METRICS_ADDR` set, `/metrics` serves request latency by route, uploads created, completed and rejected (with a reason), uploaded bytes, janitor results, the Postgres pool and Go runtime stats. Keep the listener on loopback or behind your scraper's network.

## Deployment

`deploy/files-service.service` is a systemd unit with a tight sandbox (no capabilities, `ProtectSystem=strict`, a syscall allowlist, private devices, a memory cap and `GOMEMLIMIT`). It reads settings from `/etc/files-service/env` and runs `/opt/files-service/bin/server` as an unprivileged `files` user. With Postgres on the same host the service connects over the Unix socket with peer authentication, so there is no database password to manage.

A `Dockerfile` builds static binaries on top of `distroless/static` and runs them as a non-root user. Versions are stamped with `-ldflags -X main.version` and exposed at `/version`.

## License

[MIT](LICENSE)
