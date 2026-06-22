package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

var requestLatency metric.Float64Histogram

func init() {
	meter := otel.Meter("llm-wiki-bff")
	var err error
	requestLatency, err = meter.Float64Histogram(
		"llm_wiki/request_latency",
		metric.WithDescription("Per-endpoint request latency in milliseconds"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(
			1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000,
		),
	)
	if err != nil {
		requestLatency, _ = noop.NewMeterProvider().Meter("").Float64Histogram("")
	}
}

// LatencyMiddleware records per-endpoint HTTP request latency via OpenTelemetry.
// Must be registered AFTER gin.Default() so that c.FullPath() resolves correctly.
func LatencyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		elapsed := float64(time.Since(start)) / float64(time.Millisecond)
		if c.FullPath() != "" {
			requestLatency.Record(c.Request.Context(), elapsed,
				metric.WithAttributes(
					attribute.String("endpoint", c.FullPath()),
					attribute.String("method", c.Request.Method),
				),
			)
		}
	}
}
