package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestPrometheusMiddlewareRecordsRequestMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(PrometheusMiddleware())
	router.POST("/metrics-test/:id", func(c *gin.Context) {
		c.Status(http.StatusCreated)
	})

	req := httptest.NewRequest(http.MethodPost, "/metrics-test/123", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusCreated)
	}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	counter := findMetric(t, families, "http_requests_total", map[string]string{
		"method":   http.MethodPost,
		"endpoint": "/metrics-test/:id",
		"status":   "201",
	})
	if got := counter.GetCounter().GetValue(); got != 1 {
		t.Fatalf("http_requests_total = %v, want 1", got)
	}

	histogram := findMetric(t, families, "http_request_duration_seconds", map[string]string{
		"method":   http.MethodPost,
		"endpoint": "/metrics-test/:id",
	})
	if got := histogram.GetHistogram().GetSampleCount(); got != 1 {
		t.Fatalf("histogram sample count = %d, want 1", got)
	}

	wantBounds := []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10}
	buckets := histogram.GetHistogram().GetBucket()
	if len(buckets) != len(wantBounds) {
		t.Fatalf("bucket count = %d, want %d", len(buckets), len(wantBounds))
	}
	for i, want := range wantBounds {
		if got := buckets[i].GetUpperBound(); got != want {
			t.Fatalf("bucket %d upper bound = %v, want %v", i, got, want)
		}
	}
}

func findMetric(t *testing.T, families []*dto.MetricFamily, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			gotLabels := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				gotLabels[label.GetName()] = label.GetValue()
			}
			matches := true
			for key, value := range labels {
				if gotLabels[key] != value {
					matches = false
					break
				}
			}
			if matches {
				return metric
			}
		}
	}
	t.Fatalf("metric %q with labels %#v not found", name, labels)
	return nil
}
