package observability

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/contrib/detectors/gcp"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
)

// InitMetrics sets up the OTel MeterProvider with GCP resource detection
// and the native Cloud Monitoring exporter (not generic OTLP).
func InitMetrics(ctx context.Context, serviceName, projectID string) (*sdkmetric.MeterProvider, error) {
	exporter, err := mexporter.New(
		mexporter.WithProjectID(projectID),
	)
	if err != nil {
		return nil, fmt.Errorf("metric exporter: %w", err)
	}

	// GCP resource detection, fall back to env
	res, err := resource.New(ctx,
		resource.WithDetectors(gcp.NewDetector()),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		log.Printf("[observability] resource detection failed, using env: %v", err)
		res = resource.Environment()
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter,
			sdkmetric.WithInterval(10*time.Second),
		)),
	)
	otel.SetMeterProvider(provider)
	log.Printf("[observability] OTel meter provider initialized for %s", serviceName)
	return provider, nil
}

// GetProjectID returns the GCP project ID from env or metadata.
func GetProjectID() string {
	if id := os.Getenv("GOOGLE_CLOUD_PROJECT"); id != "" {
		return id
	}
	if id := os.Getenv("GCP_PROJECT"); id != "" {
		return id
	}
	return ""
}
