/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// clusterUtil is real-node CPU/memory utilization (KWOK nodes excluded).
type clusterUtil struct {
	OK               bool
	RealNodes        int
	CPUUsedCores     float64
	CPUCapacityCores float64
	CPUPct           float64
	MemUsedMi        int64
	MemCapacityMi    int64
	MemPct           float64
}

// clusterNodeUtil is real-node usage / allocatable from metrics-server.
// OK=false if metrics-server is missing, a real node lacks a sample, or usage is invalid.
func (c *clients) clusterNodeUtil(ctx context.Context) clusterUtil {
	var u clusterUtil
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return u
	}
	real := map[string]bool{}
	var capCPUmilli, capMemMi int64
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels["type"] == "kwok" {
			continue
		}
		real[n.Name] = true
		if q, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
			capCPUmilli += q.MilliValue()
		}
		if q, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
			capMemMi += q.Value() / (1024 * 1024)
		}
	}
	if len(real) == 0 || capCPUmilli == 0 {
		return u
	}

	mc, err := metricsclient.NewForConfig(c.rest)
	if err != nil {
		return u
	}
	nm, err := mc.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return u
	}

	covered := map[string]bool{}
	var useCPUmilli, useMemMi int64
	for i := range nm.Items {
		it := &nm.Items[i]
		if !real[it.Name] {
			continue
		}
		cpu, okCPU := it.Usage[corev1.ResourceCPU]
		mem, okMem := it.Usage[corev1.ResourceMemory]
		if !okCPU || !okMem {
			continue
		}
		covered[it.Name] = true
		useCPUmilli += cpu.MilliValue()
		useMemMi += mem.Value() / (1024 * 1024)
	}
	if len(covered) != len(real) {
		return u
	}

	u.OK = true
	u.RealNodes = len(real)
	u.CPUUsedCores = float64(useCPUmilli) / 1000.0
	u.CPUCapacityCores = float64(capCPUmilli) / 1000.0
	u.CPUPct = float64(useCPUmilli) / float64(capCPUmilli)
	u.MemUsedMi = useMemMi
	u.MemCapacityMi = capMemMi
	if capMemMi > 0 {
		u.MemPct = float64(useMemMi) / float64(capMemMi)
	}
	return u
}

// summary formats real-node CPU/memory utilization for logs and artifacts.
func (u clusterUtil) summary() string {
	if !u.OK {
		return "cluster cpu/mem: unavailable (metrics-server absent or incomplete)"
	}
	return fmt.Sprintf("cluster cpu=%.1f%% (%.1f/%.1f cores) mem=%.1f%% (%d/%d Mi) over %d real nodes",
		u.CPUPct*100, u.CPUUsedCores, u.CPUCapacityCores, u.MemPct*100, u.MemUsedMi, u.MemCapacityMi, u.RealNodes)
}
