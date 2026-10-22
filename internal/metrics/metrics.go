package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/medonomator/files-service/internal/janitor"
)

const namespace = "files_service"

type Registry struct {
	registry        *prometheus.Registry
	requests        *prometheus.HistogramVec
	uploadsCreated  *prometheus.CounterVec
	uploadsReady    *prometheus.CounterVec
	uploadedBytes   *prometheus.CounterVec
	uploadsRejected *prometheus.CounterVec
	janitorRemoved  prometheus.Counter
	janitorFailed   prometheus.Counter
}

func New(version string) *Registry {
	registry := prometheus.NewRegistry()
	r := &Registry{
		registry: registry,
		requests: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request latency by route, method and status.",
			Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"route", "method", "status"}),
		uploadsCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "uploads_created_total", Help: "Upload tickets issued.",
		}, []string{"client", "mode"}),
		uploadsReady: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "uploads_completed_total", Help: "Uploads verified and marked ready.",
		}, []string{"client"}),
		uploadedBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "uploaded_bytes_total", Help: "Bytes of uploads that became ready.",
		}, []string{"client"}),
		uploadsRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "uploads_rejected_total", Help: "Uploads rejected at completion.",
		}, []string{"client", "reason"}),
		janitorRemoved: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "janitor_removed_total", Help: "Abandoned uploads removed by the janitor.",
		}),
		janitorFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "janitor_failures_total", Help: "Abandoned uploads the janitor failed to remove.",
		}),
	}

	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: "build_info", Help: "Build information.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	build.Set(1)

	for _, collector := range []prometheus.Collector{
		r.requests, r.uploadsCreated, r.uploadsReady, r.uploadedBytes, r.uploadsRejected,
		r.janitorRemoved, r.janitorFailed, build,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		registry.MustRegister(collector)
	}

	return r
}

func (r *Registry) WatchPool(pool *pgxpool.Pool) {
	gauge := func(name, help string, value func(*pgxpool.Stat) float64) {
		r.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "pgxpool", Name: name, Help: help,
		}, func() float64 { return value(pool.Stat()) }))
	}

	gauge("acquired_conns", "Connections in use.", func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) })
	gauge("idle_conns", "Idle connections.", func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) })
	gauge("max_conns", "Pool size limit.", func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) })
	gauge("empty_acquire_wait_seconds", "Time spent waiting for a free connection.", func(s *pgxpool.Stat) float64 {
		return s.EmptyAcquireWaitTime().Seconds()
	})
}

func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.registry, promhttp.HandlerOpts{Registry: r.registry})
}

func (r *Registry) ObserveRequest(route, method string, status int, duration time.Duration) {
	r.requests.WithLabelValues(route, method, strconv.Itoa(status)).Observe(duration.Seconds())
}

func (r *Registry) UploadCreated(client, mode string) {
	r.uploadsCreated.WithLabelValues(client, mode).Inc()
}

func (r *Registry) UploadCompleted(client string, sizeBytes int64) {
	r.uploadsReady.WithLabelValues(client).Inc()
	r.uploadedBytes.WithLabelValues(client).Add(float64(sizeBytes))
}

func (r *Registry) UploadRejected(client, reason string) {
	r.uploadsRejected.WithLabelValues(client, reason).Inc()
}

func (r *Registry) JanitorSwept(result janitor.Result) {
	r.janitorRemoved.Add(float64(result.Removed))
	r.janitorFailed.Add(float64(result.Failed))
}
