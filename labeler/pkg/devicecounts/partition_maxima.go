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

package devicecounts

import "sync"

// partitionMaxima keeps the learned expected count of each class and hardware
// partition between reconciliations.
//
// Learning a partition's expected count means reading every peer in it, which
// costs one CEL evaluation per peer. Doing that on every node event makes each
// event O(N) and a restart O(N^2), because the informer delivers an add event
// for every node. Instead, a partition is learned by one peer walk, and each
// reconciliation afterwards records the reconciled node's own contribution.
// Expected counts never fall automatically, so the maximum only has to be
// relearned when the node that set it leaves the partition, lowers its value or
// is deleted.
//
// It is safe for concurrent use: node, ResourceSlice and sweep reconciliations
// run on different goroutines.
type partitionMaxima struct {
	mu sync.Mutex

	partitions    map[partitionExpectedKey]*learnedPartition
	contributions map[peerCurrentCountKey]contribution
}

// learnedPartition is the expected count learned for one class and partition.
// holders counts the recorded nodes whose contribution equals max. learned is
// false until a peer walk has read every peer, and again once no recorded node
// holds max.
type learnedPartition struct {
	max     int
	holders int
	learned bool
}

// contribution is what one node adds to its partition's expected count: the
// larger of its expected count and its current count.
type contribution struct {
	partition *learnedPartition
	value     int
}

func newPartitionMaxima() *partitionMaxima {
	return &partitionMaxima{
		partitions:    make(map[partitionExpectedKey]*learnedPartition),
		contributions: make(map[peerCurrentCountKey]contribution),
	}
}

// learnWith returns a partition's expected count, learning it first with walk
// when needed. walk returns the contribution of every peer it finds in the
// partition, keyed by node name.
//
// The lock is held while walk runs, so no contribution recorded or forgotten
// by another reconciliation can be lost between reading the peers and storing
// what walk found. walk must not call back into m. A walk is needed once per
// partition and again only after the last node holding its maximum is gone,
// so other reconciliations rarely wait on it.
func (m *partitionMaxima) learnWith(key partitionExpectedKey, walk func() map[string]int) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	if partition, ok := m.partitions[key]; ok && partition.learned {
		return partition.max
	}

	m.learnLocked(key, walk())

	return m.partitions[key].max
}

func (m *partitionMaxima) learnLocked(key partitionExpectedKey, peers map[string]int) {
	partition := m.partitionLocked(key)

	// Contributions recorded since the partition was last learned may be stale,
	// for example from a node that has since left it. The walk is authoritative.
	for node, previous := range m.contributions {
		if previous.partition == partition {
			delete(m.contributions, node)
		}
	}

	partition.max, partition.holders = 0, 0

	for nodeName, value := range peers {
		node := peerCurrentCountKey{classIndex: key.classIndex, nodeName: nodeName}

		// A peer that last contributed to another partition has moved here. Its
		// old partition must lose it as a holder, or it keeps a stale maximum.
		if previous, ok := m.contributions[node]; ok {
			m.removeLocked(node, previous)
		}

		m.addLocked(node, partition, value)
	}

	partition.learned = true
}

// record stores a reconciled node's contribution to a partition and raises the
// partition's expected count when the contribution is higher.
func (m *partitionMaxima) record(key partitionExpectedKey, nodeName string, value int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	node := peerCurrentCountKey{classIndex: key.classIndex, nodeName: nodeName}
	partition := m.partitionLocked(key)

	previous, ok := m.contributions[node]
	if ok && previous.partition == partition {
		if value == previous.value {
			return
		}

		// A higher value from the same node cannot lower the maximum, so it
		// needs no relearning even when the node held the old maximum.
		if value > previous.value {
			if previous.value == partition.max {
				partition.holders--
			}

			delete(m.contributions, node)
			m.addLocked(node, partition, value)

			return
		}
	}

	if ok {
		m.removeLocked(node, previous)
	}

	m.addLocked(node, partition, value)
}

// forget removes a node's contribution to a class, for a node that is deleted
// or no longer belongs to any partition of that class.
func (m *partitionMaxima) forget(classIndex int, nodeName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	node := peerCurrentCountKey{classIndex: classIndex, nodeName: nodeName}
	if previous, ok := m.contributions[node]; ok {
		m.removeLocked(node, previous)
	}
}

func (m *partitionMaxima) partitionLocked(key partitionExpectedKey) *learnedPartition {
	partition, ok := m.partitions[key]
	if !ok {
		partition = &learnedPartition{}
		m.partitions[key] = partition
	}

	return partition
}

func (m *partitionMaxima) addLocked(node peerCurrentCountKey, partition *learnedPartition, value int) {
	m.contributions[node] = contribution{partition: partition, value: value}

	switch {
	case value > partition.max:
		partition.max, partition.holders = value, 1
	case value == partition.max:
		partition.holders++
	}
}

// removeLocked removes a node's contribution. When the last node holding the
// partition's maximum goes, the next-highest value is unknown, so the partition
// is relearned by a peer walk the next time it is needed.
func (m *partitionMaxima) removeLocked(node peerCurrentCountKey, previous contribution) {
	delete(m.contributions, node)

	partition := previous.partition
	if previous.value != partition.max {
		return
	}

	partition.holders--
	if partition.holders <= 0 {
		partition.learned = false
	}
}
