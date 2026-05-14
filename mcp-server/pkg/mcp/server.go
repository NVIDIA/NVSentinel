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
	"errors"
	"fmt"

	mcpgoserver "github.com/mark3labs/mcp-go/server"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/mcp-server/pkg/prompts"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
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
	AuthToken string

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
// mcp-go server. Tool tasks 6-16 of the merge plan each add an
// s.mcpServer.AddTool(...) call here; until then this is intentionally a
// no-op so the transport can be exercised end-to-end without tools.
func (s *Server) registerTools() error {
	// Tool registrations live here. See tasks 6-16 of the donation merge
	// plan; each adds an s.mcpServer.AddTool(...) call with a handler that
	// reads from s.store and/or s.k8sClient.
	return nil
}

// registerPrompts attaches every prompt in the donated prompts.Library to the
// mcp-go server. Prompts are static templates, so the registration is a
// straight iteration with no per-call setup.
func (s *Server) registerPrompts() {
	for _, promptDef := range prompts.Library {
		s.mcpServer.AddPrompt(promptDef.ToMCPPrompt(), promptDef.BuildHandler())
	}
}
