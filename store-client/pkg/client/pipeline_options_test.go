// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import "testing"

func TestResolvePipelineOptions(t *testing.T) {
	pipeline := []any{map[string]any{"$match": map[string]any{"nodeName": "node-a"}}}

	raw, extended := ResolvePipelineOptions(WithExtendedFilters(pipeline))
	if !extended || len(raw.([]any)) != 1 {
		t.Fatalf("extended options = %#v, %t", raw, extended)
	}

	raw, extended = ResolvePipelineOptions(pipeline)
	if extended || len(raw.([]any)) != 1 {
		t.Fatalf("plain options = %#v, %t", raw, extended)
	}

	_, options := ResolvePipelineStageOptions(WithExtendedFilterPrefix(pipeline, -1))
	if options.ExtendedFilterPrefix != 0 {
		t.Fatalf("negative prefix = %d, want 0", options.ExtendedFilterPrefix)
	}
}
