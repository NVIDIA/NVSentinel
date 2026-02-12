// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package types provides core types for gang discovery and coordination.
package types

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// GangConfigVolumeName is the volume name injected by the webhook for gang coordination.
// The controller uses this to identify pods that belong to a gang.
const GangConfigVolumeName = "nvsentinel-preflight-gang-config"

type PeerInfo struct {
	PodName   string
	PodIP     string
	NodeName  string
	Namespace string
}

type GangInfo struct {
	// GangID is the unique identifier for the gang.
	GangID string

	// ExpectedMinCount is the total number of pods expected in the gang.
	// This may be known from scheduler CRDs (e.g., Volcano's minMember,
	// K8s Workload's minCount).
	ExpectedMinCount int

	// Peers contains information about all discovered gang members.
	Peers []PeerInfo
}

// GangDiscoverer discovers all pods belonging to the same gang.
// Different schedulers (Volcano, Kueue, native K8s workloadRef) have different
// mechanisms for identifying gang members.
type GangDiscoverer interface {
	// Name returns the discoverer name (for logging/metrics).
	Name() string

	// CanHandle returns true if this discoverer can handle the given pod.
	// This is used to select the appropriate discoverer in a chain.
	CanHandle(pod *corev1.Pod) bool

	// ExtractGangID extracts the gang identifier from a pod.
	// Returns empty string if the pod doesn't belong to a gang.
	// This is a lightweight operation that doesn't require API calls.
	ExtractGangID(pod *corev1.Pod) string

	// DiscoverPeers finds all pods in the same gang.
	// This typically requires listing pods via the Kubernetes API.
	// Returns nil GangInfo if the pod doesn't belong to a gang.
	DiscoverPeers(ctx context.Context, pod *corev1.Pod) (*GangInfo, error)
}
