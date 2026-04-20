// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package otelmetrics provides OTel metric emission for preflight checks.
package otelmetrics

import (
	"context"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const defaultEndpoint = "datadog-agent.datadog.svc.cluster.local:4317"

// EmitCheckResult records a preflight check completion and flushes to the
// OTLP collector.  Best-effort — logs errors but never returns them.
func EmitCheckResult(checkName string, passed bool, attrs map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := emit(ctx, checkName, passed, attrs); err != nil {
		slog.Debug("Failed to emit OTel metric", "error", err)
	}
}

func emit(ctx context.Context, checkName string, passed bool, extraAttrs map[string]string) error {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	serviceName := os.Getenv("DD_SERVICE")
	if serviceName == "" {
		serviceName = "nvsentinel-preflight"
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	exporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithGRPCConn(conn))
	if err != nil {
		return err
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(serviceName),
	)

	provider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter, metric.WithInterval(time.Second))),
	)
	defer func() {
		_ = provider.Shutdown(ctx)
	}()

	meter := provider.Meter("nvsentinel.preflight")
	counter, err := meter.Int64Counter(
		"nvsentinel.preflight.check.completed",
		otelmetric.WithDescription("Preflight check completions"),
	)
	if err != nil {
		return err
	}

	result := "pass"
	if !passed {
		result = "fail"
	}

	otelAttrs := []attribute.KeyValue{
		attribute.String("check", checkName),
		attribute.String("result", result),
	}
	for k, v := range extraAttrs {
		otelAttrs = append(otelAttrs, attribute.String(k, v))
	}

	counter.Add(ctx, 1, otelmetric.WithAttributes(otelAttrs...))

	return provider.ForceFlush(ctx)
}
