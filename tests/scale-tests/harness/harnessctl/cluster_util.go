/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// OK=false if metrics-server is missing or no real capacity.
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

	raw, err := c.kube.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").DoRaw(ctx)
	if err != nil {
		return u
	}
	var nm struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &nm); err != nil {
		return u
	}
	var useCPUmilli, useMemMi int64
	for _, it := range nm.Items {
		if !real[it.Metadata.Name] {
			continue
		}
		if q, err := resource.ParseQuantity(it.Usage.CPU); err == nil {
			useCPUmilli += q.MilliValue()
		}
		if q, err := resource.ParseQuantity(it.Usage.Memory); err == nil {
			useMemMi += q.Value() / (1024 * 1024)
		}
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
		return "cluster cpu/mem: unavailable (metrics-server absent)"
	}
	return fmt.Sprintf("cluster cpu=%.1f%% (%.1f/%.1f cores) mem=%.1f%% (%d/%d Mi) over %d real nodes",
		u.CPUPct*100, u.CPUUsedCores, u.CPUCapacityCores, u.MemPct*100, u.MemUsedMi, u.MemCapacityMi, u.RealNodes)
}
