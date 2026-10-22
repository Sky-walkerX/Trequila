package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTP Metrics
	HttpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	HttpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	// Business Metrics
	TripsCreatedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "trips_created_total",
			Help: "Total number of trips created",
		},
	)

	TripsCompletedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "trips_completed_total",
			Help: "Total number of trips completed",
		},
	)

	TripsFailedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "trips_failed_total",
			Help: "Total number of trips failed",
		},
	)

	// SSE Metrics
	SSEConnectionsActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "sse_connections_active",
			Help: "Number of active SSE connections",
		},
	)

	// Cache Metrics
	CacheHitsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cache_hits_total",
			Help: "Total cache hits",
		},
		[]string{"cache_type"},
	)

	CacheMissesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cache_misses_total",
			Help: "Total cache misses",
		},
		[]string{"cache_type"},
	)

	// External API Metrics
	ExternalAPIRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "external_api_requests_total",
			Help: "Total external API requests",
		},
		[]string{"api", "status"},
	)

	ExternalAPIRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "external_api_request_duration_seconds",
			Help:    "External API request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"api"},
	)
)

var (
	// EventsHandledTotal counts bus deliveries by subscription and outcome
	// (ok, retry, permanent).
	EventsHandledTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bus_events_handled_total",
			Help: "Event deliveries handled, by subscription and result",
		},
		[]string{"subscription", "result"},
	)

	// EventHandleDuration times a single handler invocation.
	EventHandleDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "bus_event_handle_duration_seconds",
			Help:    "Time spent handling one event delivery",
			Buckets: []float64{.01, .05, .1, .5, 1, 5, 15, 60, 180},
		},
		[]string{"subscription"},
	)

	// OutboxPending is the number of unpublished outbox rows seen at the last flush.
	OutboxPending = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "outbox_pending",
			Help: "Unpublished outbox rows observed at the last relay flush",
		},
	)
)
