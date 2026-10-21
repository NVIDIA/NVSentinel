// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

package informer

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	cordonlabels "github.com/nvidia/nvsentinel/commons/pkg/labels"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
)

// Ownership and its claim are decided from the Node version being patched.
func (c *FaultQuarantineClient) annotationsWithCordonOwnership(node *corev1.Node,
	incoming map[string]string, cordon, alreadyQuarantined bool) (map[string]string, error) {
	annotations := make(map[string]string, len(incoming)+2)
	maps.Copy(annotations, incoming)
	delete(annotations, cordonownership.AnnotationKey)
	delete(annotations, cordonownership.ValidationSessionAnnotationKey)

	isCordonedKey := common.QuarantineHealthEventIsCordonedAnnotationKey
	if c.DryRunMode || incoming[isCordonedKey] != common.QuarantineHealthEventIsCordonedAnnotationValueTrue {
		return annotations, nil
	}

	preExistingKey := common.QuarantineHealthEventCordonPreExistingAnnotationKey
	annotations[isCordonedKey] = common.QuarantineHealthEventIsCordonedAnnotationValueTrue

	if !node.Spec.Unschedulable {
		if !cordon {
			return nil, fmt.Errorf("node %s is schedulable; recalculate the required cordon action", node.Name)
		}

		// A Node without a UID cannot anchor a claim. Its cordon then follows the
		// ownership checks that need no claim.
		if node.UID != "" {
			if _, err := cordonownership.Store(node, cordonownership.New(node)); err != nil {
				return nil, err
			}
		}

		delete(annotations, preExistingKey)
		delete(node.Annotations, preExistingKey)

		return annotations, nil
	}

	return retainCordonOwnership(node, annotations, alreadyQuarantined)
}

func retainCordonOwnership(node *corev1.Node, annotations map[string]string,
	alreadyQuarantined bool) (map[string]string, error) {
	preExistingKey := common.QuarantineHealthEventCordonPreExistingAnnotationKey
	_, dryRun := node.Annotations[common.QuarantineHealthEventDryRunAnnotationKey]
	_, cordonRecorded := node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]
	_, preExistingRecorded := node.Annotations[preExistingKey]

	previousOwnership := common.CordonOwnership(node.Annotations)
	if alreadyQuarantined && !dryRun && (cordonRecorded || preExistingRecorded) &&
		previousOwnership != common.CordonOwnershipOwned {
		// Preserve the decision already made for a manual or unknown cordon.
		// A taint-only session has not made a cordon ownership decision yet.
		if value, exists := node.Annotations[preExistingKey]; exists {
			annotations[preExistingKey] = value
		} else {
			annotations[preExistingKey] = common.QuarantineHealthEventCordonPreExistingAnnotationValue
		}

		return annotations, preserveCordonClaim(node)
	}

	if !dryRun && canRetainOwnedCordon(node, alreadyQuarantined) {
		delete(annotations, preExistingKey)
		delete(node.Annotations, preExistingKey)

		return annotations, nil
	}

	annotations[preExistingKey] = common.QuarantineHealthEventCordonPreExistingAnnotationValue

	return annotations, preserveCordonClaim(node)
}

func canRetainOwnedCordon(node *corev1.Node, alreadyQuarantined bool) bool {
	_, inherited := cordonownership.Inheritable(node)
	_, hasClaim := node.Annotations[cordonownership.AnnotationKey]
	legacyOwned := alreadyQuarantined &&
		common.CordonOwnership(node.Annotations) == common.CordonOwnershipOwned && !hasClaim

	return inherited || legacyOwned
}

func preserveCordonClaim(node *corev1.Node) error {
	claim, valid := cordonownership.Read(node)
	if !valid {
		cordonownership.Delete(node)
		return nil
	}

	claim.PreserveCordon = true
	_, err := cordonownership.Store(node, claim)

	return err
}

// Metadata-only acquisition must also conflict with concurrent ownership changes.
func (c *FaultQuarantineClient) updateNodeOwnership(ctx context.Context, name string,
	update func(*corev1.Node) error) error {
	mu, _ := c.operationMutex.LoadOrStore(name, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	nodes := &handoffNodeClient{NodeInterface: c.Clientset.CoreV1().Nodes()}

	var originalUID string

	_, err := c.nodePatcher.Patch(ctx, nodes, name, nil, func(node *corev1.Node) error {
		if originalUID != "" && originalUID != string(node.UID) {
			return fmt.Errorf("node %s identity changed during cordon ownership update", name)
		}

		originalUID = string(node.UID)
		nodes.uid, nodes.version = node.UID, node.ResourceVersion

		return update(node)
	})

	return err
}

func (c *FaultQuarantineClient) clearDryRunAnnotations(node *corev1.Node, cordon bool) {
	if c.DryRunMode {
		return
	}

	_, dryRun := node.Annotations[common.QuarantineHealthEventDryRunAnnotationKey]
	delete(node.Annotations, common.QuarantineHealthEventDryRunAnnotationKey)

	// A taint-only promotion does not acquire a cordon recorded in dry run.
	if dryRun && !cordon {
		delete(node.Annotations, common.QuarantineHealthEventIsCordonedAnnotationKey)
		delete(node.Annotations, common.QuarantineHealthEventCordonPreExistingAnnotationKey)
	}
}

// A deterministic request name permits retries only for the same node and handoff.
func validateExistingValidationRequest(existing, desired *unstructured.Unstructured) error {
	existingOwner, err := validationNodeOwner(existing)
	if err != nil {
		return err
	}

	desiredOwner, err := validationNodeOwner(desired)
	if err != nil {
		return err
	}

	if existing.GetDeletionTimestamp() != nil ||
		existingOwner.Name != desiredOwner.Name || existingOwner.UID != desiredOwner.UID {
		return fmt.Errorf("existing ValidationRequest %s has a different or deleting node identity", existing.GetName())
	}

	key := common.ValidationCordonOwnershipAnnotationKey
	ownership := desired.GetAnnotations()[key]

	switch ownership {
	case common.CordonOwnershipOwned, common.CordonOwnershipPreExisting, common.CordonOwnershipUnknown:
	default:
		return fmt.Errorf("ValidationRequest %s has no valid cordon ownership", desired.GetName())
	}

	if !sameValidationHandoff(existing, desired) {
		return fmt.Errorf("existing ValidationRequest %s has different ownership or validation targets", existing.GetName())
	}

	return nil
}

func sameValidationHandoff(existing, desired *unstructured.Unstructured) bool {
	// A request from a producer without ownership support keeps its legacy
	// semantics. A retry after an upgrade adopts it when it targets the same
	// node and tests, instead of leaving the node quarantined.
	if _, ownershipAware := existing.GetAnnotations()[common.ValidationCordonOwnershipAnnotationKey]; !ownershipAware {
		_, claimed := existing.GetAnnotations()[cordonownership.RequestClaimIDAnnotationKey]

		return !claimed && reflect.DeepEqual(existing.Object["spec"], desired.Object["spec"])
	}

	for _, key := range []string{common.ValidationCordonOwnershipAnnotationKey, common.ValidationSourceHashAnnotationKey,
		cordonownership.RequestClaimIDAnnotationKey} {
		if existing.GetAnnotations()[key] != desired.GetAnnotations()[key] {
			return false
		}
	}

	return reflect.DeepEqual(existing.Object["spec"], desired.Object["spec"])
}

func validationNodeOwner(obj *unstructured.Unstructured) (metav1.OwnerReference, error) {
	var owners []metav1.OwnerReference

	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion != "v1" || ref.Kind != "Node" {
			continue
		}

		owners = append(owners, ref)
	}

	if len(owners) != 1 || owners[0].Name == "" || owners[0].UID == "" {
		return metav1.OwnerReference{}, fmt.Errorf("ValidationRequest %s has no unambiguous Node owner", obj.GetName())
	}

	return owners[0], validateRequestTarget(obj, owners[0].Name)
}

func validateRequestTarget(obj *unstructured.Unstructured, nodeName string) error {
	nodes, found, err := unstructured.NestedSlice(obj.Object, "spec", "nodes")
	if err != nil || !found || len(nodes) != 1 {
		return fmt.Errorf("ValidationRequest %s has no unambiguous Node target", obj.GetName())
	}

	target, ok := nodes[0].(map[string]any)
	if !ok || target["name"] != nodeName {
		return fmt.Errorf("ValidationRequest %s target differs from its Node owner", obj.GetName())
	}

	return nil
}

func labelsWithoutCordonAudit(keys []string) []string {
	return slices.DeleteFunc(slices.Clone(keys), func(key string) bool {
		return strings.HasSuffix(key, cordonlabels.CordonedBySuffix) ||
			strings.HasSuffix(key, cordonlabels.CordonedReasonSuffix) ||
			strings.HasSuffix(key, cordonlabels.CordonedTimestampSuffix)
	})
}

func removeQuarantineAnnotations(node *corev1.Node, keys []string) bool {
	retireClaim := false

	for _, key := range keys {
		if key == cordonownership.AnnotationKey {
			retireClaim = true
			continue
		}

		if key != cordonownership.ValidationSessionAnnotationKey {
			delete(node.Annotations, key)
		}
	}

	return retireClaim
}

func (c *FaultQuarantineClient) finishManualCordonClaim(node *corev1.Node) error {
	if c.DryRunMode {
		return nil
	}

	if !cordonownership.HasAssociations(node) {
		cordonownership.Delete(node)
		return nil
	}

	return preserveCordonClaim(node)
}
