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

// Package pipeline provides a transformer pipeline for processing health events.
// It includes a registry-based factory for creating transformers from configuration.
package pipeline

import (
	"context"
	"log/slog"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type Transformer interface {
	Transform(ctx context.Context, event *pb.HealthEvent) error
	Name() string
}

type Pipeline struct {
	transformers []Transformer
}

func New(transformers ...Transformer) *Pipeline {
	return &Pipeline{transformers: transformers}
}

func (p *Pipeline) Process(ctx context.Context, event *pb.HealthEvent) {
	for _, t := range p.transformers {
		if err := t.Transform(ctx, event); err != nil {
			slog.Warn("Transformer failed",
				"transformer", t.Name(),
				"node", event.NodeName,
				"error", err)
		}
	}
}
