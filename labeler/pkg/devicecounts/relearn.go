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
	"log/slog"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The relearn annotation of a class records the relearn ID and the expected
// count the labeler wrote under it, for example
// nvsentinel.dgxc.nvidia.com/gpu.count.relearn: "2026-11-05-mig/4".
const (
	relearnAnnotationPrefix = "nvsentinel.dgxc.nvidia.com/"
	relearnAnnotationSuffix = ".count.relearn"
)

// RelearnAnnotationKey returns the annotation that certifies the expected
// label of a class.
func RelearnAnnotationKey(className string) string {
	return relearnAnnotationPrefix + className + relearnAnnotationSuffix
}

func relearnAnnotation(classConfig ClassConfig) string {
	if classConfig.RelearnID == "" {
		return ""
	}

	return RelearnAnnotationKey(classConfig.Name)
}

func validateRelearnID(index int, classConfig ClassConfig) error {
	if classConfig.RelearnID == "" {
		return nil
	}

	prefix := fmt.Sprintf("expectedDeviceCounts.classes[%d] (%s)", index, classConfig.Name)

	if errs := validation.IsValidLabelValue(classConfig.RelearnID); len(errs) > 0 {
		return fmt.Errorf("%s: relearnID %q: %s", prefix, classConfig.RelearnID, strings.Join(errs, "; "))
	}

	key := RelearnAnnotationKey(classConfig.Name)
	if errs := validation.IsQualifiedName(key); len(errs) > 0 {
		return fmt.Errorf("%s: class name cannot form the relearn annotation %q: %s",
			prefix, key, strings.Join(errs, "; "))
	}

	return nil
}

func relearnMarker(relearnID, expected string) string {
	return relearnID + "/" + expected
}

// expectedEvidence returns the expected label of node when it may raise a
// learned expected count, and whether the label was ignored because it was
// learned in another partition than the one observation puts the node in.
//
// A label counts only in the partition it was learned in (learnedInPartition),
// so a node that changes partition, for example when MIG is turned on or off,
// does not bring its count into the new partition. Under a relearn ID a label
// also needs a relearn annotation that records that ID and that value, so a
// value written before the relearn, by an older labeler, or by hand does not
// count until the labeler writes it again.
func (class compiledClass) expectedEvidence(node *corev1.Node, observation countObservation) (string, bool) {
	label := node.Labels[class.Labels.Expected]
	if label == "" {
		return "", false
	}

	if !class.learnedInPartition(node, observation) {
		return "", true
	}

	if class.RelearnID != "" && node.Annotations[class.relearnAnnotation] != relearnMarker(class.RelearnID, label) {
		return "", false
	}

	return label, false
}

// setExpected writes the expected label of the class, the partition annotation
// that records where it was learned, and, under a relearn ID, the annotation
// that certifies it. The caller writes them in one patch.
//
// A node without a partition keeps the partition annotation it has, because its
// label still belongs to the partition it was learned in.
func (class compiledClass) setExpected(node *corev1.Node, expected int, observation countObservation) bool {
	value := strconv.Itoa(expected)
	changed := false

	if previous := node.Labels[class.Labels.Expected]; previous != value {
		if old, ok := parseCountLabel(previous); ok && old > expected {
			slog.Info("Lowering expected device count", "node", node.Name, "class", class.Name,
				"previous", old, "expected", expected, "relearnID", class.RelearnID)
		}

		node.Labels[class.Labels.Expected] = value
		changed = true
	}

	if observation.hasPartition && setAnnotation(node, class.partitionAnnotation, observation.partitionKey) {
		changed = true
	}

	if class.RelearnID != "" && setAnnotation(node, class.relearnAnnotation, relearnMarker(class.RelearnID, value)) {
		changed = true
	}

	return changed
}

// setAnnotation sets an annotation on node and reports whether it changed.
func setAnnotation(node *corev1.Node, key, value string) bool {
	if current, ok := node.Annotations[key]; ok && current == value {
		return false
	}

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	node.Annotations[key] = value

	return true
}

// valueCertified reports whether the expected label of the class is the value
// its relearn annotation records, under any relearn ID.
func (class compiledClass) valueCertified(node *corev1.Node) bool {
	label := node.Labels[class.Labels.Expected]

	return label != "" && strings.HasSuffix(node.Annotations[class.relearnAnnotation], "/"+label)
}

// RelearnAnnotationKeys returns the relearn annotations of the classes that
// have a relearn ID. EvidenceAnnotationKeys includes them for the node cache.
func (m *Manager) RelearnAnnotationKeys() []string {
	if !m.Enabled() {
		return nil
	}

	var keys []string

	for _, class := range m.classes {
		if class.relearnAnnotation != "" {
			keys = append(keys, class.relearnAnnotation)
		}
	}

	return keys
}

// RelearnCertificationLost reports whether an update left an expected label
// that its relearn annotation certified uncertified: someone changed or
// removed the label, or removed the annotation. The labeler's own writes keep
// the label certified, so they never trigger a reconcile, and neither does an
// annotation that changes only its relearn ID.
func (m *Manager) RelearnCertificationLost(oldNode, newNode *corev1.Node) bool {
	if !m.Enabled() || oldNode == nil || newNode == nil {
		return false
	}

	for _, class := range m.classes {
		if class.RelearnID != "" && class.valueCertified(oldNode) && !class.valueCertified(newNode) {
			return true
		}
	}

	return false
}
