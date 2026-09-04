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

package controller

import (
	"fmt"
	"strings"
	"time"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	compress "github.com/NVIDIA/cluster-readiness-engine/pkg/controller/compress"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TupleKey is the deduplication identity: (node, variant, reason).
// All fields are comparable, so it can be used as a map key.
type TupleKey struct {
	Node    string
	Variant string
	Reason  string
}

// ErrorCode returns the stable, cert-independent error code: "<variant>/<reason>".
func (t TupleKey) ErrorCode() string {
	return t.Variant + "/" + t.Reason
}

// NodeCertFailure represents a certification failure tuple with its associated
// metadata used during a single sweep. It is not persisted — the annotation
// only stores keys.
type NodeCertFailure struct {
	Message  string
	CertRefs []CertRef
}

// CertRef identifies a Certification CR for contributor tracking.
type CertRef struct {
	Name      string
	Namespace string
}

func isCertificationTerminal(cert *nvcrev1alpha1.Certification) bool {
	return meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationFailed) ||
		meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationSucceeded)
}

func getCompletionTime(cert *nvcrev1alpha1.Certification) (time.Time, error) {
	// CRE keeps all terminal conditions on the cert; the one that did not
	// happen is Status=False and still carries a lastTransitionTime. Only the
	// True condition marks when the cert actually completed.
	var cond *metav1.Condition

	for _, condType := range []string{nvcrev1alpha1.CertificationFailed, nvcrev1alpha1.CertificationSucceeded} {
		if c := meta.FindStatusCondition(cert.Status.Conditions, condType); c != nil && c.Status == metav1.ConditionTrue {
			cond = c

			break
		}
	}

	if cond == nil {
		return time.Time{}, fmt.Errorf("cert %s/%s has no terminal condition (Failed or Succeeded)",
			cert.Namespace, cert.Name)
	}

	if cond.LastTransitionTime.IsZero() {
		return time.Time{}, fmt.Errorf("cert %s/%s: terminal condition %s has no lastTransitionTime",
			cert.Namespace, cert.Name, cond.Type)
	}

	return cond.LastTransitionTime.Time, nil
}

func getFailedNodesRef(cat nvcrev1alpha1.CertificationCategoryStatus) string {
	if cat.FailedNodesRef == nil {
		return ""
	}

	return cat.FailedNodesRef.Name
}

func getSucceededNodesRef(cat nvcrev1alpha1.CertificationCategoryStatus) string {
	if cat.SucceededNodesRef == nil {
		return ""
	}

	return cat.SucceededNodesRef.Name
}

// decodeSucceededNodesFromConfigMap reads the gzip-compressed, comma-separated
// node list written by CRE into a succeeded-nodes ConfigMap.
//
// CRE exports a decoder for failed nodes but not for succeeded nodes — its
// equivalent lives in the unexported controller helper mergeSucceededNodesCSV —
// so the encoding is reproduced here and must track that function.
func decodeSucceededNodesFromConfigMap(cm *corev1.ConfigMap) ([]string, error) {
	if cm == nil {
		return nil, nil
	}

	raw := cm.BinaryData[noderesults.SucceededNodesConfigMapKey]
	if len(raw) == 0 {
		return nil, nil
	}

	decoded, err := compress.GunzipString(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to decode succeeded-nodes entry: %w", err)
	}

	var names []string

	for _, name := range strings.Split(decoded, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}

	return names, nil
}
