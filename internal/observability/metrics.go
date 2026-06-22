package observability

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.opentelemetry.io/contrib/detectors/gcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// InitMetrics sets up the OTel MeterProvider with GCP resource detection
// and OTLP gRPC export to monitoring.googleapis.com.
// Returns a MeterProvider that should be Shutdown() on graceful exit.
// On failure, returns an error — caller should log and continue without metrics.
func InitMetrics(ctx context.Context, serviceName string) (*sdkmetric.MeterProvider, error) {
	res, err := resource.New(ctx,
		resource.WithDetectors(gcp.NewDetector()),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("resource detection: %w", err)
	}

	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint("monitoring.googleapis.com:443"),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
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
