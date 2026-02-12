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

// Package coordinator manages ConfigMap-based gang coordination for preflight init containers.
package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// invalidChars matches any character not allowed in DNS names (strictest rules)
	invalidChars = regexp.MustCompile(`[^a-z0-9-]`)
	// repeatedDashes matches consecutive dashes
	repeatedDashes = regexp.MustCompile(`-{2,}`)
)

const (
	// ConfigMapPrefix is the prefix for all preflight gang ConfigMaps.
	ConfigMapPrefix = "preflight-"

	// ConfigMapLabelGangID is the label for gang ID on ConfigMaps.
	ConfigMapLabelGangID = "nvsentinel.nvidia.com/gang-id"

	// ConfigMapLabelManagedBy is the label indicating this ConfigMap is managed by preflight.
	ConfigMapLabelManagedBy = "nvsentinel.nvidia.com/managed-by"

	// DataKeyExpectedCount is the ConfigMap data key for expected peer count.
	DataKeyExpectedCount = "expected_count"

	// DataKeyMasterAddr is the ConfigMap data key for the master (rank 0) IP address.
	DataKeyMasterAddr = "master_addr"

	// DataKeyMasterPort is the ConfigMap data key for the master port.
	DataKeyMasterPort = "master_port"

	// DataKeyPeers is the ConfigMap data key for the peer list.
	DataKeyPeers = "peers"

	// DataKeyGangID is the ConfigMap data key for the full gang ID.
	// This stores the unsanitized gang ID since labels have a 63-char limit.
	DataKeyGangID = "gang_id"

	// DefaultMasterPort is the default port for PyTorch distributed TCP bootstrap.
	DefaultMasterPort = 29500
)

// CoordinatorConfig contains configuration for the gang coordinator.
type CoordinatorConfig struct {
	// MasterPort is the port used for PyTorch distributed TCP bootstrap.
	// Default: 29500
	MasterPort int
}

func DefaultCoordinatorConfig() CoordinatorConfig {
	return CoordinatorConfig{
		MasterPort: DefaultMasterPort,
	}
}

// Coordinator manages ConfigMaps for gang coordination.
// It creates and updates ConfigMaps that init containers read to discover peers.
type Coordinator struct {
	client client.Client
	config CoordinatorConfig
}

func NewCoordinator(c client.Client, config CoordinatorConfig) *Coordinator {
	if config.MasterPort == 0 {
		config.MasterPort = DefaultMasterPort
	}

	return &Coordinator{
		client: c,
		config: config,
	}
}

// MaxLength is the maximum length for Kubernetes names/labels.
const MaxLength = 63

// truncateWithHash truncates a string to maxLen and appends a hash suffix for uniqueness.
// The original value is used for hash computation to ensure consistency.
func truncateWithHash(s, original string, maxLen int) string {
	hash := sha256.Sum256([]byte(original))
	hashSuffix := hex.EncodeToString(hash[:])[:8]

	truncateAt := int(math.Min(float64(maxLen-1-8), float64(len(s))))
	truncated := strings.TrimRight(s[:truncateAt], "-")

	return truncated + "-" + hashSuffix
}

// sanitizeString sanitizes a string to be valid for Kubernetes names and labels.
// Uses DNS subdomain rules (strictest): only [a-z0-9-], max 63 chars, must start/end alphanumeric.
func sanitizeString(value string) string {
	if value == "" {
		return ""
	}

	sanitized := strings.ToLower(value)
	sanitized = invalidChars.ReplaceAllString(sanitized, "-")
	sanitized = repeatedDashes.ReplaceAllString(sanitized, "-")
	sanitized = strings.Trim(sanitized, "-")

	if sanitized == "" {
		hash := sha256.Sum256([]byte(value))
		return hex.EncodeToString(hash[:])[:MaxLength]
	}

	if len(sanitized) <= MaxLength {
		return sanitized
	}

	return truncateWithHash(sanitized, value, MaxLength)
}

// ConfigMapName returns the ConfigMap name for a given gang ID.
func ConfigMapName(gangID string) string {
	name := sanitizeString(gangID)
	fullName := ConfigMapPrefix + name

	if len(fullName) <= MaxLength {
		return fullName
	}

	return ConfigMapPrefix + truncateWithHash(name, gangID, MaxLength-len(ConfigMapPrefix))
}

// EnsureConfigMap creates the gang ConfigMap if it doesn't exist.
// This should be called early (e.g., during admission) to ensure the ConfigMap
// exists before pods try to mount it.
func (c *Coordinator) EnsureConfigMap(
	ctx context.Context,
	namespace string,
	gangID string,
	expectedMinCount int,
) error {
	configMapName := ConfigMapName(gangID)
	existing := &corev1.ConfigMap{}

	err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: configMapName}, existing)
	if err == nil {
		return nil
	}

	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check ConfigMap %s: %w", configMapName, err)
	}

	gangInfo := &types.GangInfo{
		GangID:           gangID,
		ExpectedMinCount: expectedMinCount,
	}
	cm := c.createConfigMap(configMapName, namespace, gangInfo)

	err = c.client.Create(ctx, cm)
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create ConfigMap %s: %w", configMapName, err)
	}

	slog.Info("Created gang ConfigMap",
		"configMap", configMapName,
		"namespace", namespace,
		"gangID", gangID)

	return nil
}

// RegisterPeer registers a pod as a peer in the gang ConfigMap.
// Creates the ConfigMap if it doesn't exist.
func (c *Coordinator) RegisterPeer(
	ctx context.Context,
	namespace string,
	gangInfo *types.GangInfo,
	peer types.PeerInfo,
) error {
	if err := c.EnsureConfigMap(ctx, namespace, gangInfo.GangID, gangInfo.ExpectedMinCount); err != nil {
		return fmt.Errorf("failed to ensure ConfigMap: %w", err)
	}

	configMapName := ConfigMapName(gangInfo.GangID)

	slog.Debug("Registering peer in gang ConfigMap",
		"configMap", configMapName,
		"namespace", namespace,
		"gangID", gangInfo.GangID,
		"peer", peer.PodName,
		"peerIP", peer.PodIP)

	if err := c.updateConfigMap(ctx, namespace, configMapName, gangInfo.ExpectedMinCount, peer); err != nil {
		return fmt.Errorf("failed to update ConfigMap: %w", err)
	}

	slog.Info("Registered peer in gang ConfigMap",
		"configMap", configMapName,
		"namespace", namespace,
		"peer", peer.PodName,
		"peerIP", peer.PodIP)

	return nil
}

// updateConfigMap updates an existing ConfigMap, retrying on conflict.
func (c *Coordinator) updateConfigMap(
	ctx context.Context,
	namespace string,
	configMapName string,
	expectedCount int,
	peer types.PeerInfo,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm := &corev1.ConfigMap{}
		if err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: configMapName}, cm); err != nil {
			return fmt.Errorf("failed to get ConfigMap %s: %w", configMapName, err)
		}

		// Update expected_count if it was 0 (skeleton) and we now have the real value
		if expectedCount > 0 {
			currentCount, _ := strconv.Atoi(cm.Data[DataKeyExpectedCount])
			if currentCount == 0 {
				cm.Data[DataKeyExpectedCount] = strconv.Itoa(expectedCount)
			}
		}

		c.addPeerToConfigMap(cm, peer)
		c.updateMasterAddr(cm)

		return c.client.Update(ctx, cm)
	})
}

// GetGangConfigMap retrieves the gang ConfigMap.
func (c *Coordinator) GetGangConfigMap(ctx context.Context, namespace, gangID string) (*corev1.ConfigMap, error) {
	configMapName := ConfigMapName(gangID)

	cm := &corev1.ConfigMap{}
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: configMapName}, cm); err != nil {
		return nil, fmt.Errorf("failed to get ConfigMap %s: %w", configMapName, err)
	}

	return cm, nil
}

// ParsePeers parses the peers string from a ConfigMap into a slice of PeerInfo.
// Format: "podName;podIP;rank" per line (rank is optional for backwards compatibility).
func ParsePeers(peersData string) []types.PeerInfo {
	var peers []types.PeerInfo

	for line := range strings.SplitSeq(strings.TrimSpace(peersData), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ";", 3)
		if len(parts) < 2 {
			continue
		}

		peers = append(peers, types.PeerInfo{
			PodName: strings.TrimSpace(parts[0]),
			PodIP:   strings.TrimSpace(parts[1]),
		})
	}

	return peers
}

// GetRank returns the rank of a pod in the gang based on alphabetical ordering.
func GetRank(podName string, peers []types.PeerInfo) int {
	names := make([]string, len(peers))
	for i, p := range peers {
		names[i] = p.PodName
	}

	sort.Strings(names)

	for i, name := range names {
		if name == podName {
			return i
		}
	}

	return -1
}

// createConfigMap creates a new ConfigMap for gang coordination.
func (c *Coordinator) createConfigMap(name, namespace string, gangInfo *types.GangInfo) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				ConfigMapLabelGangID:    sanitizeString(gangInfo.GangID),
				ConfigMapLabelManagedBy: "preflight",
			},
		},
		Data: map[string]string{
			DataKeyExpectedCount: strconv.Itoa(gangInfo.ExpectedMinCount),
			DataKeyMasterPort:    strconv.Itoa(c.config.MasterPort),
			DataKeyPeers:         "",
			DataKeyMasterAddr:    "",
			DataKeyGangID:        gangInfo.GangID, // Store full gang ID in data
		},
	}
}

// addPeerToConfigMap adds a peer to the ConfigMap's peer list.
func (c *Coordinator) addPeerToConfigMap(cm *corev1.ConfigMap, peer types.PeerInfo) {
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}

	if peer.PodIP == "" {
		return
	}

	existingPeers := ParsePeers(cm.Data[DataKeyPeers])
	found := false

	for i, p := range existingPeers {
		if p.PodName == peer.PodName {
			existingPeers[i].PodIP = peer.PodIP
			found = true

			break
		}
	}

	if !found {
		existingPeers = append(existingPeers, peer)
	}

	// Sort peers by name for consistent ordering and rank assignment
	sort.Slice(existingPeers, func(i, j int) bool {
		return existingPeers[i].PodName < existingPeers[j].PodName
	})

	// Serialize peers with rank (index after sorting)
	var lines []string
	for rank, p := range existingPeers {
		lines = append(lines, fmt.Sprintf("%s;%s;%d", p.PodName, p.PodIP, rank))
	}

	cm.Data[DataKeyPeers] = strings.Join(lines, "\n")
}

// updateMasterAddr updates the master address in the ConfigMap.
// Master is the pod with rank 0 (first alphabetically).
func (c *Coordinator) updateMasterAddr(cm *corev1.ConfigMap) {
	peers := ParsePeers(cm.Data[DataKeyPeers])
	if len(peers) == 0 {
		return
	}

	// Sort by name to determine rank 0
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].PodName < peers[j].PodName
	})

	// Rank 0 is the master
	if peers[0].PodIP != "" {
		cm.Data[DataKeyMasterAddr] = peers[0].PodIP
	}
}
