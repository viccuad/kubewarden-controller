package metrics

import (
	"context"
	"time"

	policiesv1alpha2 "github.com/kubewarden/kubewarden-controller/apis/policies/v1alpha2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/global"
	controller "go.opentelemetry.io/otel/sdk/metric/controller/basic"
	processor "go.opentelemetry.io/otel/sdk/metric/processor/basic"
	"go.opentelemetry.io/otel/sdk/metric/selector/simple"
)

const (
	METER_NAME                        = "kubewarden"
	POLICY_COUNTER_METRIC_NAME        = "kubewarden_policy_total"
	POLICY_COUNTER_METRIC_DESCRIPTION = "How many policies are installed in the cluster"
)

func New(openTelemetryEndpoint string) error {
	ctx := context.Background()

	driver := otlpgrpc.NewDriver(
		otlpgrpc.WithInsecure(),
		otlpgrpc.WithEndpoint(openTelemetryEndpoint),
	)
	exporter, err := otlp.NewExporter(ctx, driver)
	if err != nil {
		return err
	}
	controller := controller.New(
		processor.New(
			simple.NewWithExactDistribution(),
			exporter,
		),
		controller.WithExporter(exporter),
		controller.WithCollectPeriod(2*time.Second),
	)
	global.SetMeterProvider(controller.MeterProvider())
	err = controller.Start(ctx)
	return err
}

func RecordPolicyCount(policy *policiesv1alpha2.ClusterAdmissionPolicy) {
	failurePolicy := ""
	if policy.Spec.FailurePolicy != nil {
		failurePolicy = string(*policy.Spec.FailurePolicy)
	}
	commonLabels := []attribute.KeyValue{
		attribute.String("name", policy.Name),
		attribute.String("policy_server", policy.Spec.PolicyServer),
		attribute.String("module", policy.Spec.Module),
		attribute.Bool("mutating", policy.Spec.Mutating),
		attribute.String("namespace", policy.Namespace),
		attribute.String("failure_policy", failurePolicy),
		attribute.String("policy_status", string(policy.Status.PolicyStatus)),
	}
	meter := global.Meter(METER_NAME)
	valueRecorder := metric.Must(meter).
		NewInt64Counter(
			POLICY_COUNTER_METRIC_NAME,
			metric.WithDescription(POLICY_COUNTER_METRIC_DESCRIPTION),
		).Bind(commonLabels...)
	defer valueRecorder.Unbind()
	valueRecorder.Add(context.Background(), 1)
}
