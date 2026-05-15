// Copyright 2026 k8s-gpu-mcp-server contributors
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

package prompts

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPromptDef_ToMCPPrompt(t *testing.T) {
	def := PromptDef{
		Name:        "test-prompt",
		Description: "Test prompt description",
		Arguments: []ArgumentDef{
			{Name: "arg1", Description: "First arg", Required: true},
			{Name: "arg2", Description: "Second arg", Required: false, Default: "default"},
		},
		Template: "Test {{arg1}} and {{arg2}}",
	}

	p := def.ToMCPPrompt()

	assert.Equal(t, "test-prompt", p.Name)
	assert.Equal(t, "Test prompt description", p.Description)
	assert.Len(t, p.Arguments, 2)
}

func TestPromptDef_RenderTemplate(t *testing.T) {
	tests := []struct {
		name     string
		def      PromptDef
		args     map[string]string
		expected string
	}{
		{
			name: "basic substitution",
			def: PromptDef{
				Template: "Hello {{name}}!",
			},
			args:     map[string]string{"name": "World"},
			expected: "Hello World!",
		},
		{
			name: "multiple substitutions",
			def: PromptDef{
				Template: "Check {{node}} for {{issue}}",
			},
			args:     map[string]string{"node": "gpu-1", "issue": "errors"},
			expected: "Check gpu-1 for errors",
		},
		{
			name: "default value",
			def: PromptDef{
				Arguments: []ArgumentDef{
					{Name: "node", Default: "all nodes"},
				},
				Template: "Check {{node}}",
			},
			args:     map[string]string{},
			expected: "Check all nodes",
		},
		{
			name: "missing arg without default uses empty",
			def: PromptDef{
				Arguments: []ArgumentDef{
					{Name: "node", Default: ""},
				},
				Template: "Check {{node}}",
			},
			args:     map[string]string{},
			expected: "Check",
		},
		{
			name: "undefined placeholder stays as-is",
			def: PromptDef{
				Template: "Check {{undefined}}",
			},
			args:     map[string]string{},
			expected: "Check {{undefined}}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.def.RenderTemplate(tt.args)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestPromptDef_BuildHandler(t *testing.T) {
	def := PromptDef{
		Name:        "test",
		Description: "Test prompt",
		Arguments: []ArgumentDef{
			{Name: "node", Description: "Node name", Required: false, Default: "all"},
		},
		Template: "Check {{node}} GPUs",
	}

	handler := def.BuildHandler()

	t.Run("with argument", func(t *testing.T) {
		req := mcp.GetPromptRequest{}
		req.Params.Name = "test"
		req.Params.Arguments = map[string]string{"node": "gpu-worker-1"}

		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Len(t, result.Messages, 1)
	})

	t.Run("without argument uses default", func(t *testing.T) {
		req := mcp.GetPromptRequest{}
		req.Params.Name = "test"
		req.Params.Arguments = map[string]string{}

		result, err := handler(context.Background(), req)
		require.NoError(t, err)
		require.NotNil(t, result)
	})
}

func TestPromptDef_BuildHandler_RequiredArg(t *testing.T) {
	def := PromptDef{
		Name:        "test",
		Description: "Test prompt",
		Arguments: []ArgumentDef{
			{Name: "node", Description: "Node name", Required: true},
		},
		Template: "Check {{node}}",
	}

	handler := def.BuildHandler()

	req := mcp.GetPromptRequest{}
	req.Params.Name = "test"
	req.Params.Arguments = map[string]string{} // Missing required arg

	_, err := handler(context.Background(), req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing required argument")
}

func TestPromptDef_BuildHandler_ContextCancellation(t *testing.T) {
	def := PromptDef{
		Name:        "test",
		Description: "Test prompt",
		Template:    "Check GPUs",
	}

	handler := def.BuildHandler()

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := mcp.GetPromptRequest{}
	req.Params.Name = "test"

	_, err := handler(ctx, req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled")
}

func TestGetPromptByName(t *testing.T) {
	tests := []struct {
		name     string
		lookup   string
		expected bool
	}{
		{"existing prompt", "gpu-health-check", true},
		{"another existing", "diagnose-xid-errors", true},
		{"third existing", "gpu-triage", true},
		{"non-existing", "not-a-prompt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, found := GetPromptByName(tt.lookup)
			assert.Equal(t, tt.expected, found)
			if found {
				assert.Equal(t, tt.lookup, p.Name)
			}
		})
	}
}

func TestGetAllPromptNames(t *testing.T) {
	names := GetAllPromptNames()

	assert.Len(t, names, 3)

	expected := map[string]bool{
		"gpu-health-check":    true,
		"diagnose-xid-errors": true,
		"gpu-triage":          true,
	}

	for _, name := range names {
		assert.True(t, expected[name], "unexpected prompt name: %s", name)
	}
}

func TestLibraryPrompts(t *testing.T) {
	// Ensure all library prompts are valid
	for _, p := range Library {
		t.Run(p.Name, func(t *testing.T) {
			assert.NotEmpty(t, p.Name, "prompt name is empty")
			assert.NotEmpty(t, p.Description, "prompt description is empty")
			assert.NotEmpty(t, p.Template, "prompt template is empty")

			// Test that ToMCPPrompt doesn't panic
			mcpPrompt := p.ToMCPPrompt()
			assert.Equal(t, p.Name, mcpPrompt.Name)

			// Test that handler can be built
			handler := p.BuildHandler()
			assert.NotNil(t, handler)
		})
	}
}

func TestGPUHealthCheckPrompt(t *testing.T) {
	p, found := GetPromptByName("gpu-health-check")
	require.True(t, found)

	t.Run("default node value", func(t *testing.T) {
		result := p.RenderTemplate(map[string]string{})
		assert.Contains(t, result, "all nodes")
		assert.Contains(t, result, "GPU Health Check")
		assert.Contains(t, result, "gpu_inventory")
		assert.Contains(t, result, "gpu_health")
	})

	t.Run("custom node value", func(t *testing.T) {
		result := p.RenderTemplate(map[string]string{"node": "gpu-worker-5"})
		assert.Contains(t, result, "gpu-worker-5")
		assert.NotContains(t, result, "all nodes")
	})
}

func TestDiagnoseXIDErrorsPrompt(t *testing.T) {
	p, found := GetPromptByName("diagnose-xid-errors")
	require.True(t, found)

	t.Run("default time_range", func(t *testing.T) {
		result := p.RenderTemplate(map[string]string{})
		assert.Contains(t, result, "24h")
		assert.Contains(t, result, "XID Error Diagnosis")
		assert.Contains(t, result, "analyze_xid")
	})

	t.Run("custom time_range", func(t *testing.T) {
		result := p.RenderTemplate(map[string]string{"time_range": "7d"})
		assert.Contains(t, result, "7d")
	})
}

func TestGPUTriagePrompt(t *testing.T) {
	p, found := GetPromptByName("gpu-triage")
	require.True(t, found)

	t.Run("defaults", func(t *testing.T) {
		result := p.RenderTemplate(map[string]string{})
		assert.Contains(t, result, "cluster-wide")
		assert.Contains(t, result, "GPU Triage Report")
		assert.Contains(t, result, "gpu_inventory")
		assert.Contains(t, result, "gpu_health")
		assert.Contains(t, result, "analyze_xid")
		assert.Contains(t, result, "pod_gpu_allocation")
	})

	t.Run("with incident_id", func(t *testing.T) {
		result := p.RenderTemplate(map[string]string{
			"node":        "gpu-worker-42",
			"incident_id": "INC-12345",
		})
		assert.Contains(t, result, "gpu-worker-42")
		assert.Contains(t, result, "INC-12345")
	})
}
