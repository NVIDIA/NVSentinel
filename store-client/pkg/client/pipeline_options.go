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

package client

// PipelineOptions carries opt-in behavior without changing the pipeline seen
// by providers that do not need it.
type PipelineOptions struct {
	Pipeline              any
	EnableExtendedFilters bool
	ExtendedFilterPrefix  int
}

// WithExtendedFilters enables MongoDB-compatible logical and field-presence
// filters in PostgreSQL. Existing consumers retain their current semantics
// unless they opt in explicitly.
func WithExtendedFilters(pipeline any) PipelineOptions {
	return PipelineOptions{Pipeline: pipeline, EnableExtendedFilters: true}
}

// WithExtendedFilterPrefix enables extended PostgreSQL translation only for
// the leading aggregation stages owned by the caller. Later configured stages
// retain legacy translation semantics.
func WithExtendedFilterPrefix(pipeline any, stages int) PipelineOptions {
	return PipelineOptions{Pipeline: pipeline, ExtendedFilterPrefix: max(stages, 0)}
}

// ResolvePipelineOptions returns the underlying pipeline and whether extended
// PostgreSQL filter translation was requested.
func ResolvePipelineOptions(pipeline any) (any, bool) {
	pipeline, options := ResolvePipelineStageOptions(pipeline)

	return pipeline, options.EnableExtendedFilters
}

// ResolvePipelineStageOptions returns the underlying pipeline and its
// stage-scoped PostgreSQL filter options.
func ResolvePipelineStageOptions(pipeline any) (any, PipelineOptions) {
	options, ok := pipeline.(PipelineOptions)
	if !ok {
		return pipeline, PipelineOptions{}
	}

	return options.Pipeline, options
}

func (o PipelineOptions) extendedFiltersForStage(stage int) bool {
	return o.EnableExtendedFilters || stage < o.ExtendedFilterPrefix
}

func (o PipelineOptions) rejectExtendedOperatorsForStage(stage int) bool {
	return !o.EnableExtendedFilters && o.ExtendedFilterPrefix > 0 && stage >= o.ExtendedFilterPrefix
}
