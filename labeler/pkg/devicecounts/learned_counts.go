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

// learnedCounts remembers the expected device count of each partition, so that
// labeler does not have to work it out again on every node event.
//
// A node's expected count is the highest count seen on any node in its
// partition (for example, all p5 nodes with H100 GPUs). Working it out means
// reading every node in the partition and running the CEL expression on each
// one. Labeler used to do that for every node event. On a fleet of 5,000
// identical nodes that is 5,000 evaluations per event, and a restart, which
// delivers an add event for every node, costs 25 million.
//
// Instead, labeler reads the whole partition once and then keeps the result up
// to date as single nodes change:
//
//   - When a node reports a higher count, the expected count goes up straight
//     away.
//   - Expected counts never go down on their own, so a lower count from a
//     node usually changes nothing, as long as another node still has the
//     highest count.
//   - The one case labeler cannot handle this way is when the last node with
//     the highest count goes away, by being deleted, moving to another
//     partition or reporting less. The next-highest count is unknown, so the
//     partition is read again the next time it is needed.
//
// For example, a partition holds node-a with 8 GPUs and node-b with 4. The
// expected count is 8, and one node has it. If node-c joins with 16, the
// expected count becomes 16 at once. If node-c is then deleted, no node is
// known to have 16, so labeler reads the partition again and finds 8.
//
// It is safe for concurrent use: node events, ResourceSlice events and the
// startup sweep reconcile on different goroutines.
type learnedCounts struct {
	mu sync.Mutex

	partitions map[partitionExpectedKey]*partitionState
	nodes      map[peerCurrentCountKey]nodeEntry
}

// partitionState is what labeler knows about one partition of one class.
type partitionState struct {
	// key is the partition's key in learnedCounts.partitions, so an empty
	// partition can be removed from the map.
	key partitionExpectedKey

	// members is how many nodes labeler remembers in the partition. When the
	// last one goes, the partition is removed, so the map does not keep every
	// grouping-label value that a node has ever had.
	members int

	// expected is the highest count of any node in the partition.
	expected int

	// nodesAtExpected is how many nodes have a count equal to expected. When it
	// drops to zero, expected may be too high, so known becomes false.
	nodesAtExpected int

	// known is true while expected can be trusted. It is false until the
	// partition has been read in full, and again after the last node with the
	// highest count goes away.
	known bool
}

// nodeEntry is what labeler remembers about one node for one class: its
// partition, and its count, which is the larger of the node's expected count
// label and its current count.
type nodeEntry struct {
	partition *partitionState
	count     int
}

func newLearnedCounts() *learnedCounts {
	return &learnedCounts{
		partitions: make(map[partitionExpectedKey]*partitionState),
		nodes:      make(map[peerCurrentCountKey]nodeEntry),
	}
}

// getOrLearn returns the expected count of a partition. If labeler does not
// know it yet, it calls readPeers, which reads every node in the partition and
// returns each node's count by node name, and remembers the result.
//
// The lock is held while readPeers runs. Otherwise another goroutine could
// record a newer count for a node while readPeers is still reading, and the
// older count that readPeers returns would overwrite it. readPeers must not
// call back into c. Reading a partition is rare, once at startup and again
// only after the last node with the highest count goes away, so other
// reconciliations seldom wait for the lock.
func (c *learnedCounts) getOrLearn(key partitionExpectedKey, readPeers func() map[string]int) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if partition, ok := c.partitions[key]; ok && partition.known {
		return partition.expected
	}

	c.learnLocked(key, readPeers())

	return c.partitions[key].expected
}

// learnLocked replaces everything labeler remembers about a partition with the
// counts read from its nodes.
func (c *learnedCounts) learnLocked(key partitionExpectedKey, peerCounts map[string]int) {
	partition := c.partitionLocked(key)

	// The fresh read replaces what labeler remembered, which may include nodes
	// that have left the partition since.
	for node, entry := range c.nodes {
		if entry.partition == partition {
			delete(c.nodes, node)
		}
	}

	partition.members, partition.expected, partition.nodesAtExpected = 0, 0, 0

	for nodeName, count := range peerCounts {
		c.setNodeLocked(peerCurrentCountKey{classIndex: key.classIndex, nodeName: nodeName}, partition, count)
	}

	partition.known = true
}

// setNode remembers a node's latest count in its partition, raising the
// partition's expected count when the node's count is higher.
func (c *learnedCounts) setNode(key partitionExpectedKey, nodeName string, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.setNodeLocked(peerCurrentCountKey{classIndex: key.classIndex, nodeName: nodeName}, c.partitionLocked(key), count)
}

// removeNode forgets a node, because it was deleted or no longer belongs to any
// partition of the class.
func (c *learnedCounts) removeNode(classIndex int, nodeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	node := peerCurrentCountKey{classIndex: classIndex, nodeName: nodeName}
	if entry, ok := c.nodes[node]; ok {
		delete(c.nodes, node)
		c.uncountLocked(entry)
	}
}

func (c *learnedCounts) partitionLocked(key partitionExpectedKey) *partitionState {
	partition, ok := c.partitions[key]
	if !ok {
		partition = &partitionState{key: key}
		c.partitions[key] = partition
	}

	return partition
}

// setNodeLocked counts the node's new entry before it uncounts the old one.
// In that order, a node that raises its own count, or moves to another
// partition, never leaves a partition looking as if nobody has its expected
// count when somebody still does.
func (c *learnedCounts) setNodeLocked(node peerCurrentCountKey, partition *partitionState, count int) {
	previous, hadPrevious := c.nodes[node]
	if hadPrevious && previous.partition == partition && previous.count == count {
		return
	}

	c.nodes[node] = nodeEntry{partition: partition, count: count}
	partition.members++

	switch {
	case count > partition.expected:
		partition.expected, partition.nodesAtExpected = count, 1
	case count == partition.expected:
		partition.nodesAtExpected++
	}

	if hadPrevious {
		c.uncountLocked(previous)
	}
}

// uncountLocked takes a node's old entry out of its partition's tally. If it
// was the partition's last node, the partition is removed. If it was the last
// node with the partition's expected count, labeler no longer knows the
// partition's highest count, so the partition will be read again the next
// time it is needed.
func (c *learnedCounts) uncountLocked(entry nodeEntry) {
	partition := entry.partition

	partition.members--
	if partition.members <= 0 {
		// Delete only the entry that still points at this partition. A full read
		// may already have replaced it.
		if c.partitions[partition.key] == partition {
			delete(c.partitions, partition.key)
		}

		return
	}

	if entry.count != partition.expected {
		return
	}

	partition.nodesAtExpected--
	if partition.nodesAtExpected <= 0 {
		partition.known = false
	}
}
