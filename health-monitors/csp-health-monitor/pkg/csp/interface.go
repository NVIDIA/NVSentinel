// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package csp

import (
	"context"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// Monitor defines the methods a CSP client must implement.
type Monitor interface {
	// StartMonitoring initiates the process of watching for maintenance events.
	// It takes a context for cancellation and a channel to send normalized events.
	StartMonitoring(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error

	// GetName returns the name of the CSP (e.g., "gcp", "aws").
	GetName() model.CSP
}
