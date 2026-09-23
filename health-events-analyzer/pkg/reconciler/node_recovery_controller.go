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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

type nodeRecoveryController struct {
	client    kubernetes.Interface
	informer  cache.SharedIndexInformer
	nodes     corelisters.NodeLister
	queue     workqueue.TypedRateLimitingInterface[string]
	ready     chan struct{}
	keys      map[string]bool
	reconcile func(context.Context, string) error
}

func newNodeRecoveryController(kube kubernetes.Interface, rules *config.TomlConfig,
	reconcile func(context.Context, string) error,
) (*nodeRecoveryController, error) {
	informer := coreinformers.NewNodeInformer(kube, 0, cache.Indexers{})
	c := &nodeRecoveryController{
		client: kube, informer: informer, nodes: corelisters.NewNodeLister(informer.GetIndexer()),
		ready: make(chan struct{}), keys: make(map[string]bool), reconcile: reconcile,
		queue: workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	for _, rule := range rules.Rules {
		if rule.EvaluateRule && rule.Recovery != nil && rule.Recovery.AnnotationKey != "" {
			c.keys[rule.Recovery.AnnotationKey] = true
		}
	}

	if err := informer.SetTransform(c.transformNode); err != nil {
		c.queue.ShutDown()
		return nil, fmt.Errorf("set recovery node transform: %w", err)
	}

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueChanged(nil, obj) },
		UpdateFunc: c.enqueueChanged,
	})
	if err != nil {
		c.queue.ShutDown()
		return nil, fmt.Errorf("register recovery node handler: %w", err)
	}

	return c, nil
}

func (c *nodeRecoveryController) transformNode(obj any) (any, error) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return obj, nil
	}

	annotations := make(map[string]string)

	for key := range c.keys {
		if value, exists := node.Annotations[key]; exists {
			annotations[key] = value
		}
	}

	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: node.Name, UID: node.UID, ResourceVersion: node.ResourceVersion,
		CreationTimestamp: node.CreationTimestamp, Annotations: annotations,
	}}, nil
}

func (c *nodeRecoveryController) enqueueChanged(oldObj, newObj any) {
	node, ok := newObj.(*corev1.Node)
	if !ok {
		return
	}

	previous, _ := oldObj.(*corev1.Node)
	for key := range c.keys {
		changed := previous == nil || previous.UID != node.UID || previous.Annotations[key] != node.Annotations[key]
		if node.Annotations[key] != "" && changed {
			c.queue.Add(node.Name)
			return
		}
	}
}

func (c *nodeRecoveryController) run(ctx context.Context, workers int) error {
	var workersDone sync.WaitGroup
	defer workersDone.Wait()
	defer c.queue.ShutDown()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ctx = runCtx

	workersDone.Go(func() { c.informer.Run(ctx.Done()) })

	syncCtx, stopSync := context.WithTimeout(ctx, 30*time.Second)
	defer stopSync()

	if !cache.WaitForCacheSync(syncCtx.Done(), c.informer.HasSynced) {
		return fmt.Errorf("sync recovery node cache: %w", syncCtx.Err())
	}

	close(c.ready)

	for range max(1, workers) {
		workersDone.Go(func() {
			for c.processNext(ctx) {
			}
		})
	}

	<-ctx.Done()

	return nil
}

func (c *nodeRecoveryController) processNext(ctx context.Context) bool {
	node, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(node)

	if ctx.Err() != nil {
		c.queue.Forget(node)
		return false
	}

	if err := c.reconcile(ctx, node); err != nil {
		slog.ErrorContext(ctx, "Node recovery request failed; retaining annotation for retry", "node", node, "error", err)
		c.queue.AddRateLimited(node)
	} else {
		c.queue.Forget(node)
	}

	return true
}

func (c *nodeRecoveryController) removeRequest(ctx context.Context, node *corev1.Node, key, value string) error {
	path := "/metadata/annotations/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")

	patch, err := json.Marshal([]annotationPatchOperation{
		{Op: "test", Path: "/metadata/uid", Value: string(node.UID)},
		{Op: "test", Path: path, Value: value},
		{Op: "remove", Path: path},
	})
	if err != nil {
		return fmt.Errorf("encode recovery annotation cleanup: %w", err)
	}

	_, err = c.client.CoreV1().Nodes().Patch(ctx, node.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	// An operator may replace the request while its transition is being stored.
	// Only inspect the API after a failed conditional patch, not during polling.
	current, getErr := c.client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) {
		return nil
	}

	if getErr == nil && (current.UID != node.UID || current.Annotations[key] != value) {
		return nil
	}

	return fmt.Errorf("remove completed recovery annotation: %w", err)
}

type annotationPatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value,omitempty"`
}
