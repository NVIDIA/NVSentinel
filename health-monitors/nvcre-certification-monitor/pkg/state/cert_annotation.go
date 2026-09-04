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

package state

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const (
	CertProcessedKey  = "nvsentinel.dgxc.nvidia.com/cert-processed"
	ErrorRecoveredKey = "nvsentinel.dgxc.nvidia.com/error-recovered"
)

// CertAnnotationHelper reads and writes the cert-processed and error-recovered
// annotations on Certification CRs.
type CertAnnotationHelper struct {
	client client.Client
}

// NewCertAnnotationHelper constructs a CertAnnotationHelper.
func NewCertAnnotationHelper(c client.Client) *CertAnnotationHelper {
	return &CertAnnotationHelper{client: c}
}

// legacyProcessedValue is what releases before the timestamp format wrote.
// It is treated as processed so an upgrade does not republish every cert.
const legacyProcessedValue = "true"

// IsProcessed reports whether the monitor has already published for the
// cert's current terminal state. The annotation holds the terminal
// condition's lastTransitionTime at publish time; a stored value older than
// terminalTime means CRE reopened the cert and it finished again since.
func (h *CertAnnotationHelper) IsProcessed(cert *nvcrev1alpha1.Certification, terminalTime time.Time) bool {
	raw, ok := cert.GetAnnotations()[CertProcessedKey]
	if !ok {
		return false
	}

	if raw == legacyProcessedValue {
		return true
	}

	processedAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		slog.Warn("Unparseable cert-processed annotation, treating cert as unprocessed",
			"cert", cert.Name, "namespace", cert.Namespace, "value", raw)

		return false
	}

	return !processedAt.Before(terminalTime)
}

// SetProcessed records the terminal transition time the monitor published for.
func (h *CertAnnotationHelper) SetProcessed(
	ctx context.Context, certName, certNamespace string, terminalTime time.Time,
) error {
	// A newer stamp means the cert reached a new terminal state, so the
	// error-recovered list written for the previous state goes with it.
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q,%q:null}}}`,
		CertProcessedKey, terminalTime.UTC().Format(time.RFC3339), ErrorRecoveredKey)

	return h.patch(ctx, certName, certNamespace, CertProcessedKey, patch)
}

// IsRecovered checks if a specific tuple has been marked as operator-recovered on this cert.
func (h *CertAnnotationHelper) IsRecovered(cert *nvcrev1alpha1.Certification, tupleKey string) bool {
	annotations := cert.GetAnnotations()
	if annotations == nil {
		return false
	}

	raw, ok := annotations[ErrorRecoveredKey]
	if !ok || raw == "" {
		return false
	}

	var keys []string

	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		slog.Warn("Failed to parse error-recovered annotation",
			"cert", cert.Name, "namespace", cert.Namespace, "error", err)

		return false
	}

	for _, k := range keys {
		if k == tupleKey {
			return true
		}
	}

	return false
}

// AddRecovered appends a tuple key to the cert's error-recovered annotation.
func (h *CertAnnotationHelper) AddRecovered(ctx context.Context, certName, certNamespace, tupleKey string) error {
	cert := &nvcrev1alpha1.Certification{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: certName, Namespace: certNamespace}, cert); err != nil {
		return fmt.Errorf("failed to get cert %s/%s: %w", certNamespace, certName, err)
	}

	var existing []string
	if raw, ok := cert.GetAnnotations()[ErrorRecoveredKey]; ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &existing); err != nil {
			slog.Warn("Failed to parse error-recovered annotation, starting fresh",
				"cert", certName, "namespace", certNamespace, "error", err)

			existing = nil
		}
	}

	for _, k := range existing {
		if k == tupleKey {
			return nil
		}
	}

	existing = append(existing, tupleKey)

	b, err := json.Marshal(existing)
	if err != nil {
		return fmt.Errorf("failed to marshal error-recovered: %w", err)
	}

	return h.patchAnnotation(ctx, certName, certNamespace, ErrorRecoveredKey, string(b))
}

func (h *CertAnnotationHelper) patchAnnotation(ctx context.Context, certName, certNamespace, key, value string) error {
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value)

	return h.patch(ctx, certName, certNamespace, key, patch)
}

func (h *CertAnnotationHelper) patch(ctx context.Context, certName, certNamespace, key, patch string) error {
	cert := &nvcrev1alpha1.Certification{}
	cert.SetName(certName)
	cert.SetNamespace(certNamespace)

	if err := h.client.Patch(ctx, cert, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return fmt.Errorf("failed to patch annotation %s on cert %s/%s: %w", key, certNamespace, certName, err)
	}

	return nil
}
