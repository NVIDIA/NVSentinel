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

package dedup

import (
	"context"
	"fmt"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
)

func init() {
	pipeline.Register(Name, newFromConfig)
}

func newFromConfig(cfg *pipeline.Config, opts pipeline.Options) (pipeline.Transformer, error) {
	dedupCfg, err := LoadConfig(cfg.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load dedup configuration: %w", err)
	}

	if err := dedupCfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid dedup configuration: %w", err)
	}

	tracker := newTracker(dedupCfg.SuppressionWindow)
	//nolint:gosec // cancel is owned by the returned transformer and invoked by Pipeline.Close.
	ctx, cancel := context.WithCancel(context.Background())
	startEvictExpired(ctx, tracker, dedupCfg.CleanupInterval)

	return NewDeduplicator(tracker, dedupCfg.IncludeChecks, cancel), nil
}
