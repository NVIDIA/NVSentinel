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

package devicecounts

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The partition annotation of a class records the partition key of the node
// when the labeler wrote its expected label, for example
// nvsentinel.dgxc.nvidia.com/gpu.count.partition:
// "source=gfd|node.kubernetes.io/instance-type=gb200|nvidia.com/gpu.product=NVIDIA-GB200".
const partitionAnnotationSuffix = ".count.partition"

// PartitionAnnotationKey returns the annotation that records the partition in
// which the expected label of a class was learned.
func PartitionAnnotationKey(className string) string {
	return relearnAnnotationPrefix + className + partitionAnnotationSuffix
}

// validatePartitionAnnotation checks that the class name forms a valid
// annotation key. Every enabled class writes the annotation.
func validatePartitionAnnotation(index int, classConfig ClassConfig) error {
	key := PartitionAnnotationKey(classConfig.Name)
	if errs := validation.IsQualifiedName(key); len(errs) > 0 {
		return fmt.Errorf("expectedDeviceCounts.classes[%d] (%s): class name cannot form the partition annotation %q: %s",
			index, classConfig.Name, key, strings.Join(errs, "; "))
	}

	return nil
}

// learnedInPartition reports whether the expected label of node was learned in
// the partition that observation puts the node in now. A label written before
// the annotation existed counts, and so does the label of a node that has no
// partition, since there is nothing to compare it with.
//
// A change between two sources of the class, such as a GPU Operator mode
// change, also changes the partition key. The expected count is kept across
// it, as #2108 does, so a label recorded by another source of the class
// counts too.
func (class compiledClass) learnedInPartition(node *corev1.Node, observation countObservation) bool {
	recorded, ok := node.Annotations[class.partitionAnnotation]
	if !ok || !observation.hasPartition || recorded == observation.partitionKey {
		return true
	}

	return class.recordedByAnotherSource(recorded, observation.source)
}

// recordedByAnotherSource reports whether a recorded partition key belongs to a
// source of the class other than current.
func (class compiledClass) recordedByAnotherSource(recorded string, current *compiledSource) bool {
	if current == nil {
		return false
	}

	for _, source := range class.sources {
		if source.Name == current.Name {
			continue
		}

		prefix := sourcePartitionPrefix(source.Name)
		if recorded == prefix || strings.HasPrefix(recorded, prefix+partitionKeySeparator) {
			return true
		}
	}

	return false
}

// EvidenceAnnotationKeys returns the annotations that decide whether an
// expected label counts: the partition annotation of every class and the
// relearn annotation of each class with a relearn ID. The node cache keeps
// them.
func (m *Manager) EvidenceAnnotationKeys() []string {
	if !m.Enabled() {
		return nil
	}

	keys := make([]string, 0, len(m.classes))
	for _, class := range m.classes {
		keys = append(keys, class.partitionAnnotation)
	}

	return append(keys, m.RelearnAnnotationKeys()...)
}
