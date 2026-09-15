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

package overrides

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/commons/pkg/celevent"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type compiledRule struct {
	name     string
	filter   *celevent.Filter
	override Override
}

func compileRules(config *Config) ([]compiledRule, error) {
	if !config.Enabled || len(config.Rules) == 0 {
		return nil, nil
	}

	compiled := make([]compiledRule, 0, len(config.Rules))

	for i, rule := range config.Rules {
		filter, err := celevent.Compile(rule.When)
		if err != nil {
			slog.ErrorContext(context.Background(), "Failed to compile CEL expression",
				"rule", rule.Name, "error", err)

			return nil, fmt.Errorf("rule[%d] (%s): %w", i, rule.Name, err)
		}

		compiled = append(compiled, compiledRule{
			name:     rule.Name,
			filter:   filter,
			override: rule.Override,
		})
	}

	return compiled, nil
}

func (r *compiledRule) evaluate(ctx context.Context, event *pb.HealthEvent) (bool, error) {
	matched, err := r.filter.Matches(event)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to evaluate CEL expression", "rule", r.name, "error", err)

		return false, fmt.Errorf("rule %s: %w", r.name, err)
	}

	return matched, nil
}
