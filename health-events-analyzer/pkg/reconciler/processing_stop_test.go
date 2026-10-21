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

package reconciler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"

	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

type stubEventProcessor struct {
	start func(ctx context.Context) error
}

func (p *stubEventProcessor) Start(ctx context.Context) error     { return p.start(ctx) }
func (p *stubEventProcessor) Stop(context.Context) error          { return nil }
func (p *stubEventProcessor) SetEventHandler(client.EventHandler) {}

func newStopTestReconciler(recovery bool, start func(context.Context) error) *Reconciler {
	cfg := HealthEventsAnalyzerReconcilerConfig{HealthEventsAnalyzerRules: &config.TomlConfig{}}
	if recovery {
		cfg.HealthEventsAnalyzerRules.Rules = []config.HealthEventsAnalyzerRule{annotationRule()}
		cfg.KubernetesClient = fake.NewClientset()
	}

	return &Reconciler{config: cfg, eventProcessor: &stubEventProcessor{start: start}}
}

func TestProcessUntilCancelled(t *testing.T) {
	boom := errors.New("boom")

	for _, recovery := range []bool{false, true} {
		name := "recovery off"
		if recovery {
			name = "recovery on"
		}

		t.Run(name, func(t *testing.T) {
			t.Run("processor stops while running", func(t *testing.T) {
				r := newStopTestReconciler(recovery, func(context.Context) error { return nil })
				require.ErrorIs(t, r.processUntilCancelled(t.Context()), errEventProcessingStopped)
			})

			t.Run("processor fails while running", func(t *testing.T) {
				r := newStopTestReconciler(recovery, func(context.Context) error { return boom })
				err := r.processUntilCancelled(t.Context())
				require.ErrorIs(t, err, errEventProcessingStopped)
				require.ErrorIs(t, err, boom)
			})

			t.Run("shutdown", func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				r := newStopTestReconciler(recovery, func(ctx context.Context) error {
					cancel()
					<-ctx.Done()

					return nil
				})
				require.NoError(t, r.processUntilCancelled(ctx))
			})
		})
	}
}
