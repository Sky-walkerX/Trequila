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

	// SSE Metrics
	SSEConnectionsActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "sse_connections_active",
			Help: "Number of active SSE connections",
		},
	)

	SSEEventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sse_events_total",
			Help: "Total SSE events sent",
		},
		[]string{"event_type"},
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

	// NATS Metrics
	NatsMessagesPublished = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nats_messages_published_total",
			Help: "Total NATS messages published",
		},
		[]string{"subject"},
	)

	NatsMessagesConsumed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nats_messages_consumed_total",
			Help: "Total NATS messages consumed",
		},
		[]string{"subject"},
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
