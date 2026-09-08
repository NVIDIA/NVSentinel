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
	"time"

	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/nvcre"
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

// terminalCondition returns the True Failed or Succeeded condition, or nil
// when the cert has not reached a terminal state or its status cannot be read.
func terminalCondition(cert *unstructured.Unstructured) (*metav1.Condition, error) {
	status, err := nvcre.GetStatus(cert)
	if err != nil {
		return nil, err
	}

	// CRE keeps all terminal conditions on the cert; the one that did not
	// happen is Status=False and still carries a lastTransitionTime. Only the
	// True condition marks when the cert actually completed.
	for _, condType := range []string{nvcre.CertificationFailed, nvcre.CertificationSucceeded} {
		if c := meta.FindStatusCondition(status.Conditions, condType); c != nil && c.Status == metav1.ConditionTrue {
			return c, nil
		}
	}

	return nil, nil
}

func getCompletionTime(cert *unstructured.Unstructured) (time.Time, error) {
	cond, err := terminalCondition(cert)
	if err != nil {
		return time.Time{}, err
	}

	if cond == nil {
		return time.Time{}, fmt.Errorf("cert %s/%s has no terminal condition (Failed or Succeeded)",
			cert.GetNamespace(), cert.GetName())
	}

	if cond.LastTransitionTime.IsZero() {
		return time.Time{}, fmt.Errorf("cert %s/%s: terminal condition %s has no lastTransitionTime",
			cert.GetNamespace(), cert.GetName(), cond.Type)
	}

	return cond.LastTransitionTime.Time, nil
}

func getFailedNodesRef(cat nvcre.CategoryStatus) string {
	if cat.FailedNodesRef == nil {
		return ""
	}

	return cat.FailedNodesRef.Name
}

func getSucceededNodesRef(cat nvcre.CategoryStatus) string {
	if cat.SucceededNodesRef == nil {
		return ""
	}

	return cat.SucceededNodesRef.Name
}
