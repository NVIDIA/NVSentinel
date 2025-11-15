// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package helpers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/api"
	v1prometheus "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// CheckPrometheusMetricEmpty executes the Prometheus `query` and verifies that it returns no results,
// using `description` for error reporting.
func CheckPrometheusMetricEmpty(ctx context.Context, t *testing.T, query string, description string) error {
	result, err := queryPrometheus(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to query prometheus for %s: %w", description, err)
	}

	var resultCount int

	switch v := result.(type) {
	case model.Vector:
		resultCount = len(v)
	case model.Matrix:
		resultCount = len(v)
	case *model.Scalar:
		if v != nil {
			resultCount = 1
		}
	case *model.String:
		if v != nil {
			resultCount = 1
		}
	}

	if resultCount > 0 {
		return fmt.Errorf("%s metric query returned %d results, expected 0", description, resultCount)
	}

	return nil
}

// queryPrometheus executes the specified Prometheus `query` and returns the raw result value.
func queryPrometheus(ctx context.Context, query string) (model.Value, error) {
	client, err := api.NewClient(api.Config{
		Address: "http://localhost:9090",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus client: %w", err)
	}

	promClient := v1prometheus.NewAPI(client)

	result, warnings, err := promClient.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to query prometheus: %w", err)
	}

	if len(warnings) > 0 {
		for _, warning := range warnings {
			fmt.Printf("Prometheus query warning: %s\n", warning)
		}
	}

	return result, nil
}
