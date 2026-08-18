/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

const kwokNodeLabel = "type=kwok"

type clients struct {
	rest    *rest.Config
	kube    *kubernetes.Clientset
	dynamic dynamic.Interface
}

// newClients builds clients from in-cluster config or the caller's kubeconfig.
func newClients() (*clients, error) {
	var restCfg *rest.Config
	var err error

	if inCluster, e := rest.InClusterConfig(); e == nil {
		restCfg = inCluster
	} else {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
	}
	restCfg.QPS = 200
	restCfg.Burst = 400

	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	return &clients{rest: restCfg, kube: kube, dynamic: dyn}, nil
}

// buildNode constructs a GPU-shaped fake KWOK node object for the given index.
func buildNode(cfg Config, idx int) *corev1.Node {
	name := fmt.Sprintf("%s-%d", cfg.NodePrefix, idx)
	gpu := fmt.Sprintf("%d", cfg.GPUCount)
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:                    resource.MustParse(cfg.NodeCPU),
		corev1.ResourceMemory:                 resource.MustParse(cfg.NodeMemory),
		corev1.ResourcePods:                   resource.MustParse(fmt.Sprintf("%d", cfg.NodeMaxPods)),
		corev1.ResourceName("nvidia.com/gpu"): resource.MustParse(gpu),
	}
	spec := corev1.NodeSpec{
		Taints: []corev1.Taint{{
			Key:    "kwok.x-k8s.io/node",
			Value:  "fake",
			Effect: corev1.TaintEffectNoSchedule,
		}},
	}
	// Synthetic providerID: cloud-node-lifecycle deletes provider-less Nodes.
	if cfg.ProviderIDScheme != "" {
		spec.ProviderID = cfg.ProviderIDScheme + "://" + name
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				"kwok.x-k8s.io/node":           "fake",
				"node.alpha.kubernetes.io/ttl": "0",
			},
			Labels: map[string]string{
				"type":                                "kwok",
				"kubernetes.io/hostname":              name,
				"kubernetes.io/os":                    "linux",
				"kubernetes.io/arch":                  "amd64",
				"node-role.kubernetes.io/worker":      "",
				"nvidia.com/gpu.present":              "true",
				"nvidia.com/gpu.count":                gpu,
				"nvidia.com/gpu.product":              "NVIDIA-H100-80GB-HBM3",
				"nvidia.com/gpu.deploy.driver":        "true",
				"nvidia.com/gpu.deploy.dcgm":          "true",
				"nvidia.com/gpu.deploy.device-plugin": "true",
			},
		},
		Spec: spec,
		Status: corev1.NodeStatus{
			Capacity:    capacity,
			Allocatable: capacity,
			NodeInfo: corev1.NodeSystemInfo{
				Architecture:    "amd64",
				OperatingSystem: "linux",
				KubeletVersion:  "kwok",
				BootID:          "00000000-0000-0000-0000-000000000000",
			},
			Phase: corev1.NodeRunning,
		},
	}
}

// nodeCreateBackoff retries Create through APF throttling.
var nodeCreateBackoff = wait.Backoff{
	Duration: 200 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.2,
	Steps:    6,
	Cap:      10 * time.Second,
}

// isTransientCreateErr is true for retryable API errors. AlreadyExists is not transient.
func isTransientCreateErr(err error) bool {
	return apierrors.IsTooManyRequests(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsInternalError(err) ||
		apierrors.IsServiceUnavailable(err)
}

// listKwokNodeNames returns existing KWOK node names for gap-filling scale.
func (c *clients) listKwokNodeNames(ctx context.Context) map[string]struct{} {
	set := make(map[string]struct{})
	list, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: kwokNodeLabel})
	if err != nil {
		warnf("list kwok nodes: %v", err)
		return set
	}
	for i := range list.Items {
		set[list.Items[i].Name] = struct{}{}
	}
	return set
}

// missingIndices returns indices in [0, NodeCount) whose node name is absent.
func missingIndices(cfg Config, existing map[string]struct{}) []int {
	out := make([]int, 0)
	for i := 0; i < cfg.NodeCount; i++ {
		name := fmt.Sprintf("%s-%d", cfg.NodePrefix, i)
		if _, ok := existing[name]; !ok {
			out = append(out, i)
		}
	}
	return out
}

// scaleNodes brings the KWOK fleet to cfg.NodeCount (idempotent, gap-filling).
// Returns (created, existingAtStart, failed).
func (c *clients) scaleNodes(ctx context.Context, cfg Config) (int, int, int) {
	existing := c.listKwokNodeNames(ctx)
	infof("existing kwok nodes: %d; target: %d", len(existing), cfg.NodeCount)
	existingAtStart := len(existing)

	conc := cfg.NodeBatch
	if conc < 1 {
		conc = 100
	}

	const maxPasses = 8
	var totalCreated, lastFailed int64
	prevMissing := -1
	for pass := 1; pass <= maxPasses; pass++ {
		missing := missingIndices(cfg, existing)
		if len(missing) == 0 {
			break
		}
		if len(missing) == prevMissing {
			warnf("scale pass %d made no progress (%d still missing); stopping", pass, len(missing))
			break
		}
		if pass > 1 {
			infof("scale pass %d: %d nodes still missing, retrying", pass, len(missing))
		}
		prevMissing = len(missing)

		created, failed := c.createNodes(ctx, cfg, missing, conc)
		totalCreated += created
		lastFailed = failed
		if ctx.Err() != nil {
			break
		}
		existing = c.listKwokNodeNames(ctx)
	}
	return int(totalCreated), existingAtStart, int(lastFailed)
}

// createNodes creates the given indices with bounded concurrency, returning
// (created, failed). AlreadyExists is not counted as failure.
func (c *clients) createNodes(ctx context.Context, cfg Config, indices []int, conc int) (int64, int64) {
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var created, failed int64
	total := len(indices)

	for _, i := range indices {
		select {
		case <-ctx.Done():
			warnf("scale interrupted")
			wg.Wait()
			return created, failed
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.createOneNode(ctx, cfg, idx); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return
				}
				if n := atomic.AddInt64(&failed, 1); n <= 10 {
					warnf("create node %d: %v", idx, err)
				}
				return
			}
			if n := atomic.AddInt64(&created, 1); n%2000 == 0 {
				infof("  created %d/%d nodes this pass", n, total)
			}
		}(i)
	}
	wg.Wait()
	return created, failed
}

func (c *clients) createOneNode(ctx context.Context, cfg Config, idx int) error {
	node := buildNode(cfg, idx)
	if err := retry.OnError(nodeCreateBackoff, isTransientCreateErr, func() error {
		_, e := c.kube.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		return e
	}); err != nil {
		return err
	}
	// Best-effort status touch-up (capacity may be ignored on Create); non-fatal.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := c.kube.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur.Status.Capacity = node.Status.Capacity
		cur.Status.Allocatable = node.Status.Allocatable
		cur.Status.NodeInfo = node.Status.NodeInfo
		_, err = c.kube.CoreV1().Nodes().UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		return err
	}); err != nil {
		warnf("node %s created but status touch-up failed (non-fatal): %v", node.Name, err)
	}
	return nil
}

// waitNodesReady waits until target KWOK nodes are Ready (or timeout).
// onStall runs on readiness stall (e.g. kwok-controller restart for missed ADDs).
func (c *clients) waitNodesReady(ctx context.Context, target int, timeout time.Duration, onStall func() error) (int, bool) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		c.kube, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = kwokNodeLabel }),
	)
	nodeInformer := factory.Core().V1().Nodes()
	lister := nodeInformer.Lister()
	stop := make(chan struct{})
	defer close(stop)
	factory.Start(stop)
	if !cache.WaitForCacheSync(stop, nodeInformer.Informer().HasSynced) {
		warnf("node informer cache did not sync; falling back to direct list")
	}

	const maxStallHeals = 3
	const onStallAfter = 90 * time.Second
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	last := 0
	best := 0
	lastProgress := time.Now()
	heals := 0
	for {
		ready := countReady(lister)
		if ready != last {
			infof("kwok nodes Ready: %d/%d", ready, target)
			last = ready
		}
		// Progress = best-ever ready (not last sample); lease flaps near target.
		if ready > best {
			best = ready
			lastProgress = time.Now()
		}
		if ready >= target {
			return ready, true
		}
		if onStall != nil && heals < maxStallHeals && time.Since(lastProgress) >= onStallAfter {
			warnf("node readiness stalled at %d/%d for %s — restarting kwok-controller to force a full re-list (heal %d/%d)",
				ready, target, onStallAfter, heals+1, maxStallHeals)
			if err := onStall(); err != nil {
				warnf("stall heal failed: %v", err)
			}
			heals++
			lastProgress = time.Now()
		}
		if time.Now().After(deadline) {
			return ready, false
		}
		select {
		case <-ctx.Done():
			return ready, false
		case <-tick.C:
		}
	}
}

func countReady(lister interface {
	List(labels.Selector) ([]*corev1.Node, error)
}) int {
	nodes, err := lister.List(labels.Everything())
	if err != nil {
		return 0
	}
	ready := 0
	for _, n := range nodes {
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready++
				break
			}
		}
	}
	return ready
}

// restartKwokController refreshes kwok-controller's informer cache after create stalls.
func (c *clients) restartKwokController(ctx context.Context, cfg Config) error {
	if err := c.rolloutRestart(ctx, cfg.KWOKNamespace, "kwok-controller"); err != nil {
		return err
	}
	ok, err := c.waitRolloutComplete(ctx, cfg.KWOKNamespace, "kwok-controller", 2*time.Minute)
	if err != nil {
		return err
	}
	if ok {
		infof("kwok-controller restarted (clean cache)")
	} else {
		infof("kwok-controller restart issued (rollout still settling)")
	}
	return nil
}
