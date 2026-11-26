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

// Package nvhvclient calls NVHV's VerdictService.Evaluate without a generated
// client. It reads the verdict schema from the NVHV it talks to through gRPC
// reflection, builds the request and reads the reply with dynamic messages, and
// returns plain verdict values. A schema that lacks a field the monitor needs is
// reported as an error, so a change in NVHV shows up at once, not as a missed
// fault.
package nvhvclient

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

// ErrIncompatibleSchema means NVHV's schema lacks something the monitor needs.
var ErrIncompatibleSchema = errors.New("incompatible NVHV verdict schema")

const serviceName = "health.verdict.v1.VerdictService"

// Client calls NVHV over an existing connection.
type Client struct {
	conn grpc.ClientConnInterface

	mu     sync.Mutex
	schema *schema // read from NVHV on first use, and again after a failed call
}

// New returns a Client that calls NVHV on conn.
func New(conn grpc.ClientConnInterface) *Client {
	return &Client{conn: conn}
}

// Evaluate asks NVHV for the node's verdict with its default request.
func (c *Client) Evaluate(ctx context.Context) (*verdict.Verdict, error) {
	s, err := c.loadSchema(ctx)
	if err != nil {
		return nil, err
	}

	req, resp := dynamicpb.NewMessage(s.request), dynamicpb.NewMessage(s.response)

	if err := c.conn.Invoke(ctx, s.method, req, resp); err != nil {
		// NVHV may have been replaced by a version with another schema.
		c.forgetSchema()

		return nil, fmt.Errorf("evaluate: %w", err)
	}

	return s.decode(resp.ProtoReflect()), nil
}

func (c *Client) loadSchema(ctx context.Context) (*schema, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.schema != nil {
		return c.schema, nil
	}

	files, err := fetchFiles(ctx, c.conn, serviceName)
	if err != nil {
		return nil, err
	}

	s, err := newSchema(files)
	if err != nil {
		return nil, err
	}

	c.schema = s

	return s, nil
}

func (c *Client) forgetSchema() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.schema = nil
}
