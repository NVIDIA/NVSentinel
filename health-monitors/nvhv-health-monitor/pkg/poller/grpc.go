// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package poller

import (
	"context"
	"fmt"

	healthv1 "github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdictpb/health/v1"
)

// GRPCEvaluator calls NVHV's VerdictService.Evaluate.
type GRPCEvaluator struct {
	Client healthv1.VerdictServiceClient
}

// Evaluate asks NVHV for the node's verdict with its default request.
func (e *GRPCEvaluator) Evaluate(ctx context.Context) (*healthv1.NodeVerdict, error) {
	verdict, err := e.Client.Evaluate(ctx, &healthv1.EvaluateRequest{})
	if err != nil {
		return nil, fmt.Errorf("evaluate: %w", err)
	}

	return verdict, nil
}
