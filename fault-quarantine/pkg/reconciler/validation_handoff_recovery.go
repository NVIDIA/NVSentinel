// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
)

const validationHandoffRetryInterval = 30 * time.Second

type validationHandoffRecovery struct {
	queue     workqueue.TypedRateLimitingInterface[string]
	reconcile func(context.Context, string) (bool, error)
}

func (r *Reconciler) startValidationHandoffRecovery(ctx context.Context) (func(), error) {
	if r.config.DryRun || !r.config.TomlConfig.Validation.PersistHandoff || r.validationClient == nil {
		return func() {}, nil
	}

	recovery := &validationHandoffRecovery{
		queue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		reconcile: r.validationClient.EnsurePendingHandoff,
	}
	nodes := r.k8sClient.NodeInformer.GetInformer()

	// Registering after validationClient is initialized replays the existing
	// informer cache as Add events, including handoffs left across a restart.
	handler, err := nodes.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: recovery.enqueue,
		UpdateFunc: func(_, current any) {
			recovery.enqueue(current)
		},
	})
	if err != nil {
		recovery.queue.ShutDown()
		return nil, fmt.Errorf("watch pending validation handoffs: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		for recovery.processNext(runCtx) {
		}
	}()

	return func() {
		cancel()
		recovery.queue.ShutDown()
		<-done

		if err := nodes.RemoveEventHandler(handler); err != nil {
			slog.ErrorContext(ctx, "Remove validation handoff watcher", "error", err)
		}
	}, nil
}

func (r *validationHandoffRecovery) enqueue(obj any) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return
	}

	_, pending := node.Annotations[common.QuarantineValidationHealthEventAnnotationKey]
	_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]

	if pending && !quarantined {
		r.queue.Add(node.Name)
	}
}

func (r *validationHandoffRecovery) processNext(ctx context.Context) bool {
	name, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(name)

	if ctx.Err() != nil {
		r.queue.Forget(name)
		return false
	}

	pending, err := r.reconcile(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "Validation handoff recovery failed; retaining source for retry", "node", name, "error", err)
		r.queue.AddRateLimited(name)

		return true
	}

	r.queue.Forget(name)

	if pending {
		// Request deletion does not change the Node. Keep revisiting its source
		// until lifecycle-manager atomically registers and consumes it.
		r.queue.AddAfter(name, validationHandoffRetryInterval)
	}

	return true
}
