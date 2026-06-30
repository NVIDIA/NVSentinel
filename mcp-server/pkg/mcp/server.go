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

// Package mcp is the NVSentinel MCP server: a thin streamable-HTTP transport
// around mark3labs/mcp-go that exposes NVSentinel's read-only health surface
// (HealthEventStore + Kubernetes API) as MCP tools. The Config struct is the
// single dependency-injection boundary: callers wire a store.Reader (and
// optionally a kubernetes.Interface) and the server constructs the underlying
// mcp-go server. Tools are registered via Server.registerTools (currently a
// stub; tool tasks 6-16 of the merge plan populate it).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpgoserver "github.com/mark3labs/mcp-go/server"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/mcp-server/pkg/prompts"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/tools"
)

// Config carries everything Server needs from the caller. The zero value is
// not valid; New validates required fields.
type Config struct {
	// Version is the build version reported on /version and to the mcp-go
	// server name. Empty defaults to "dev".
	Version string

	// GitCommit is the git SHA of the build, surfaced on /version for ops
	// visibility. Optional.
	GitCommit string

	// HTTPAddr is the listen address for the streamable-HTTP transport, e.g.
	// ":8080". Required.
	HTTPAddr string

	// AuthToken, when non-empty, requires every /mcp request to carry an
	// "Authorization: Bearer <token>" header. Empty disables auth (only
	// appropriate for in-cluster networking with NetworkPolicy enforcement).
	AuthToken string `json:"-"`

	// Store is the read-only health-event view tools depend on. Required.
	Store store.Reader

	// K8sClient is used by tools that consult the Kubernetes API directly
	// (node describe, pod allocation, pod failure). nil disables those tools
	// — they may still register but will return a structured "k8s API not
	// configured" error.
	K8sClient kubernetes.Interface

	// TLS, when non-nil and Enabled(), terminates TLS on HTTPAddr using the
	// referenced cert/key files. The cert reloader picks up rotations
	// without a restart.
	TLS *TLSConfig
}

// Server is the long-lived MCP transport. Construct with New, drive with Run,
// stop with Shutdown.
type Server struct {
	mcpServer  *mcpgoserver.MCPServer
	httpAddr   string
	version    string
	authToken  string
	store      store.Reader
	k8sClient  kubernetes.Interface
	tlsConfig  *TLSConfig
	httpServer *HTTPServer
}

// New validates the Config and builds a Server with its mcp-go MCPServer and
// (empty, for now) tool registrations. It does not bind any sockets — Run
// does that.
func New(cfg Config) (*Server, error) {
	if cfg.HTTPAddr == "" {
		return nil, errors.New("mcp: Config.HTTPAddr is required")
	}

	if cfg.Store == nil {
		return nil, errors.New("mcp: Config.Store is required")
	}

	version := cfg.Version
	if version == "" {
		version = "dev"
	}

	mcpServer := mcpgoserver.NewMCPServer(
		"nvsentinel-mcp-server",
		version,
		mcpgoserver.WithPromptCapabilities(true),
	)

	s := &Server{
		mcpServer: mcpServer,
		httpAddr:  cfg.HTTPAddr,
		version:   version,
		authToken: cfg.AuthToken,
		store:     cfg.Store,
		k8sClient: cfg.K8sClient,
		tlsConfig: cfg.TLS,
	}

	if err := s.registerTools(); err != nil {
		return nil, fmt.Errorf("mcp: register tools: %w", err)
	}

	s.registerPrompts()

	return s, nil
}

// Run starts the streamable-HTTP transport and blocks until ctx is cancelled
// or the listener errors. It returns the listener error (or nil on a clean
// context-driven shutdown).
func (s *Server) Run(ctx context.Context) error {
	s.httpServer = NewHTTPServer(s.mcpServer, s.httpAddr, s.version)
	s.httpServer.SetAuthToken(s.authToken)
	s.httpServer.SetTLSConfig(s.tlsConfig)

	if err := s.httpServer.ListenAndServe(ctx); err != nil {
		return fmt.Errorf("mcp: serve: %w", err)
	}

	return nil
}

// Shutdown initiates a graceful shutdown of the underlying HTTP server. It is
// safe to call before Run has been invoked (no-op in that case).
func (s *Server) Shutdown() error {
	if s.httpServer == nil {
		return nil
	}

	return s.httpServer.Shutdown()
}

// registerTools is the single place tool definitions are attached to the
// mcp-go server. Each tool task in the merge plan adds one registration
// helper below and a call from registerTools.
func (s *Server) registerTools() error {
	s.registerGPUInventoryTool()
	s.registerGPUHealthTool()
	s.registerDescribeGPUNodeTool()
	s.registerPodGPUAllocationTool()
	s.registerPodFailureTool()
	s.registerExplainFailureTool()
	s.registerGetIncidentReportTool()
	s.registerAnalyzeXIDTool()
	s.registerGetNVLinkTopologyTool()
	s.registerGetGPUTimelineTool()

	return nil
}

// registerGPUInventoryTool wires the gpu_inventory MCP tool. It returns the
// per-node list of GPUs derived from health events (entitiesImpacted of type
// GPU), collapsed to the latest event per UUID.
func (s *Server) registerGPUInventoryTool() {
	handler := tools.NewGPUInventoryHandler(s.store)

	tool := mcpgo.NewTool(
		"gpu_inventory",
		mcpgo.WithDescription(
			"List GPUs observed in NVSentinel's health-event stream for a given Kubernetes node, "+
				"with the latest event per GPU (status, message, error codes).",
		),
		mcpgo.WithString(
			"node",
			mcpgo.Required(),
			mcpgo.Description("Kubernetes node name to query."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.GPUInventoryInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("gpu_inventory failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal gpu_inventory response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerGPUHealthTool wires the gpu_health MCP tool. It returns per-GPU
// health summaries (latest state plus event-count aggregates) for a node,
// optionally narrowed to a single GPU UUID.
func (s *Server) registerGPUHealthTool() {
	handler := tools.NewGPUHealthHandler(s.store)

	tool := mcpgo.NewTool(
		"gpu_health",
		mcpgo.WithDescription(
			"Summarise health for GPUs on a node: latest event state plus per-GPU aggregate "+
				"event and unhealthy-event counts. Optionally narrow to a single GPU UUID.",
		),
		mcpgo.WithString(
			"node",
			mcpgo.Required(),
			mcpgo.Description("Kubernetes node name to query."),
		),
		mcpgo.WithString(
			"gpu_uuid",
			mcpgo.Description("Optional GPU UUID; when set, the response contains at most one entry."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.GPUHealthInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("gpu_health failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal gpu_health response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerDescribeGPUNodeTool wires the describe_gpu_node MCP tool. It pairs
// the latest store event for a node with a flattened Kubernetes Node
// description (labels, taints, conditions, GPU capacity).
func (s *Server) registerDescribeGPUNodeTool() {
	handler := tools.NewDescribeGPUNodeHandler(s.store, s.k8sClient)

	tool := mcpgo.NewTool(
		"describe_gpu_node",
		mcpgo.WithDescription(
			"Describe a GPU node by pairing NVSentinel's latest health event with the "+
				"Kubernetes Node object (labels, taints, conditions, GPU capacity).",
		),
		mcpgo.WithString(
			"node",
			mcpgo.Required(),
			mcpgo.Description("Kubernetes node name to describe."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.DescribeGPUNodeInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("describe_gpu_node failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal describe_gpu_node response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerPodGPUAllocationTool wires the pod_gpu_allocation MCP tool. It
// lists pods requesting GPUs across an optional namespace/node scope and
// resolves their assigned UUIDs from NVIDIA_VISIBLE_DEVICES.
func (s *Server) registerPodGPUAllocationTool() {
	handler := tools.NewPodGPUAllocationHandler(s.k8sClient)

	tool := mcpgo.NewTool(
		"pod_gpu_allocation",
		mcpgo.WithDescription(
			"List pods requesting GPUs across an optional namespace and/or node scope, with "+
				"the per-pod GPU request count and (when available) the resolved GPU UUIDs.",
		),
		mcpgo.WithString(
			"namespace",
			mcpgo.Description("Optional namespace; empty lists across all namespaces."),
		),
		mcpgo.WithString(
			"node",
			mcpgo.Description("Optional node name; empty lists across all nodes."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.PodGPUAllocationInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("pod_gpu_allocation failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal pod_gpu_allocation response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerPodFailureTool wires the pod_failure MCP tool. It pairs a pod's
// K8s Pod and Event objects with NVSentinel store health events that name
// the pod in entitiesImpacted.
func (s *Server) registerPodFailureTool() {
	handler := tools.NewPodFailureHandler(s.store, s.k8sClient)

	tool := mcpgo.NewTool(
		"pod_failure",
		mcpgo.WithDescription(
			"Diagnose a failing pod: phase + restart count from the Kubernetes API, "+
				"K8s Events scoped to the pod, and NVSentinel store health events that name "+
				"the pod in entitiesImpacted.",
		),
		mcpgo.WithString(
			"pod",
			mcpgo.Required(),
			mcpgo.Description("Pod name to diagnose."),
		),
		mcpgo.WithString(
			"namespace",
			mcpgo.Required(),
			mcpgo.Description("Pod namespace."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.PodFailureInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("pod_failure failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal pod_failure response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerExplainFailureTool wires the explain_failure MCP tool. It runs
// the donated pattern matcher over recent events for a node (optionally
// narrowed to one GPU) and returns a short narrative plus the full list of
// scored pattern matches.
func (s *Server) registerExplainFailureTool() {
	handler := tools.NewExplainFailureHandler(s.store)

	tool := mcpgo.NewTool(
		"explain_failure",
		mcpgo.WithDescription(
			"Diagnose recent failures on a GPU node by running the donated pattern matcher "+
				"over store events in the last N minutes. Returns a human narrative plus the "+
				"full sorted list of matched patterns with confidence and recommendations.",
		),
		mcpgo.WithString(
			"node",
			mcpgo.Required(),
			mcpgo.Description("Kubernetes node name to diagnose."),
		),
		mcpgo.WithString(
			"gpu_uuid",
			mcpgo.Description("Optional GPU UUID; when set, only events mentioning this GPU are considered."),
		),
		mcpgo.WithNumber(
			"since_minutes",
			mcpgo.Description("Time window in minutes (default 60)."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.ExplainFailureInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("explain_failure failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal explain_failure response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerGetIncidentReportTool wires the get_incident_report MCP tool. It
// looks up the analyzer-synthesized event with the requested incident_id,
// pairs it with same-node related events around the incident time, and
// surfaces pattern-matched recommendations.
func (s *Server) registerGetIncidentReportTool() {
	handler := tools.NewGetIncidentReportHandler(s.store)

	tool := mcpgo.NewTool(
		"get_incident_report",
		mcpgo.WithDescription(
			"Look up a single NVSentinel-synthesized incident by id, returning the title, "+
				"severity, affected nodes/GPUs, related raw events, and recommended actions "+
				"from the donated pattern matcher.",
		),
		mcpgo.WithString(
			"incident_id",
			mcpgo.Required(),
			mcpgo.Description("The HealthEvent.id of the analyzer-synthesized incident event."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.GetIncidentReportInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("get_incident_report failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal get_incident_report response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerAnalyzeXIDTool wires the analyze_xid MCP tool. It queries the
// store for events tagged with the requested XID code, attributes them to
// nodes/GPUs, and pairs with the matching donor pattern (xid_79_bus_error,
// nvlink_failure, ecc_failure, etc.).
func (s *Server) registerAnalyzeXIDTool() {
	handler := tools.NewAnalyzeXIDHandler(s.store)

	tool := mcpgo.NewTool(
		"analyze_xid",
		mcpgo.WithDescription(
			"Look up every NVSentinel health event tagged with a given numeric XID code "+
				"(e.g., 79 for GPU bus fall-off, 74 for NVLink, 48/63/64 for ECC). Returns "+
				"affected nodes/GPUs, recent events, and the matching diagnostic pattern.",
		),
		mcpgo.WithNumber(
			"xid_code",
			mcpgo.Required(),
			mcpgo.Description("Numeric XID code to look up (positive integer)."),
		),
		mcpgo.WithString(
			"node",
			mcpgo.Description("Optional node name to narrow the search to one node."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.AnalyzeXIDInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("analyze_xid failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal analyze_xid response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerGetNVLinkTopologyTool wires the get_nvlink_topology MCP tool as a
// stub. Per AUDIT § 3, NVSentinel does not persist NVLink topology in the
// store; the tool returns the NVSENTINEL_DATA_GAP envelope until a monitor
// extension lands (AUDIT § 6.1).
func (s *Server) registerGetNVLinkTopologyTool() {
	handler := tools.NewGetNVLinkTopologyHandler()

	tool := mcpgo.NewTool(
		"get_nvlink_topology",
		mcpgo.WithDescription(
			"Return the NVLink topology for a node. STUB: NVSentinel does not yet persist "+
				"per-node NVLink topology in its store. The tool returns a structured "+
				"NVSENTINEL_DATA_GAP envelope so MCP clients can detect the gap.",
		),
		mcpgo.WithString(
			"node",
			mcpgo.Required(),
			mcpgo.Description("Kubernetes node name."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.GetNVLinkTopologyInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("get_nvlink_topology failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal get_nvlink_topology response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerGetGPUTimelineTool wires the get_gpu_timeline MCP tool. It
// returns the chronologically-ordered list of health events for a node,
// optionally narrowed to one GPU, within a time window.
func (s *Server) registerGetGPUTimelineTool() {
	handler := tools.NewGetGPUTimelineHandler(s.store)

	tool := mcpgo.NewTool(
		"get_gpu_timeline",
		mcpgo.WithDescription(
			"Return health events for a node in chronological order (ascending by store insertion "+
				"time), optionally narrowed to one GPU UUID and a time window in minutes.",
		),
		mcpgo.WithString(
			"node",
			mcpgo.Required(),
			mcpgo.Description("Kubernetes node name."),
		),
		mcpgo.WithString(
			"gpu_uuid",
			mcpgo.Description("Optional GPU UUID; when set, only events mentioning this GPU appear."),
		),
		mcpgo.WithNumber(
			"since_minutes",
			mcpgo.Description("Time window in minutes (default 60)."),
		),
	)

	s.mcpServer.AddTool(tool, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var in tools.GetGPUTimelineInput
		if err := req.BindArguments(&in); err != nil {
			return mcpgo.NewToolResultErrorFromErr("invalid arguments", err), nil
		}

		out, err := handler.Handle(ctx, in)
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("get_gpu_timeline failed", err), nil
		}

		fallback, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return mcpgo.NewToolResultErrorFromErr("marshal get_gpu_timeline response", err), nil
		}

		return mcpgo.NewToolResultStructured(out, string(fallback)), nil
	})
}

// registerPrompts attaches every prompt in the donated prompts.Library to the
// mcp-go server. Prompts are static templates, so the registration is a
// straight iteration with no per-call setup.
func (s *Server) registerPrompts() {
	for _, promptDef := range prompts.Library {
		s.mcpServer.AddPrompt(promptDef.ToMCPPrompt(), promptDef.BuildHandler())
	}
}
