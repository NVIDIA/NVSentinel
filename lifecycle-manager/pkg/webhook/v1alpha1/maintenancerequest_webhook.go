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

package v1alpha1

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

var mrLog = slog.With("webhook", "maintenancerequest")

// MaintenanceRequestValidator validates MaintenanceRequest objects.
// +kubebuilder:object:generate=false
type MaintenanceRequestValidator struct {
	Enabled bool
	Client  client.Client
}

func (v *MaintenanceRequestValidator) ValidateCreate(ctx context.Context,
	obj *v1alpha1.MaintenanceRequest) (admission.Warnings, error) {
	mrLog.Info("Validating MaintenanceRequest on create", "name", obj.Name)

	if !v.Enabled {
		return nil, fmt.Errorf("MaintenanceRequest controller is disabled")
	}

	if obj.Spec == nil || obj.Spec.HealthEvent == nil {
		return nil, fmt.Errorf("spec.healthEvent is required")
	}

	he := obj.Spec.HealthEvent

	if he.NodeName == "" {
		return nil, fmt.Errorf("spec.healthEvent.nodeName is required")
	}

	if he.IsHealthy {
		return nil, fmt.Errorf("spec.healthEvent.isHealthy must be false for an opening event")
	}

	if err := v.checkNodeExists(ctx, he.NodeName); err != nil {
		return nil, err
	}

	if err := v.checkNoDuplicateMR(ctx, he.NodeName, obj.Name); err != nil {
		return nil, err
	}

	if !isStartTimeInFuture(obj.Spec.StartTime) {
		return nil, fmt.Errorf("spec.startTime must be in the future")
	}

	return nil, nil
}

func (v *MaintenanceRequestValidator) ValidateUpdate(_ context.Context,
	oldObj, newObj *v1alpha1.MaintenanceRequest) (admission.Warnings, error) {
	mrLog.Info("Validating MaintenanceRequest on update", "name", newObj.Name)

	if !v.Enabled {
		return nil, fmt.Errorf("MaintenanceRequest controller is disabled")
	}

	// An update that nils out spec or healthEvent would violate the
	// same required-field invariant that ValidateCreate enforces.
	if newObj.Spec == nil || newObj.Spec.HealthEvent == nil {
		return nil, fmt.Errorf("spec.healthEvent is required")
	}

	if newObj.Spec.HealthEvent.NodeName == "" {
		return nil, fmt.Errorf("spec.healthEvent.nodeName is required")
	}

	if newObj.Spec.HealthEvent.IsHealthy {
		return nil, fmt.Errorf("spec.healthEvent.isHealthy must be false for an opening event")
	}

	if oldObj.Spec != nil {
		if err := checkUserFieldsImmutable(oldObj.Spec.HealthEvent, newObj.Spec.HealthEvent); err != nil {
			return nil, err
		}

		if !timestampsEqual(oldObj.Spec.StartTime, newObj.Spec.StartTime) {
			if !isStartTimeInFuture(newObj.Spec.StartTime) {
				return nil, fmt.Errorf("spec.startTime must be in the future when changed")
			}
		}
	}

	return nil, nil
}

func (v *MaintenanceRequestValidator) ValidateDelete(_ context.Context,
	obj *v1alpha1.MaintenanceRequest) (admission.Warnings, error) {
	mrLog.Info("Validating MaintenanceRequest on delete", "name", obj.Name)

	return nil, nil
}

// checkNoDuplicateMR is a best-effort early rejection of a second MR for the
// same node. It uses a List, so two concurrent creates can both pass; the
// controller's annotation-based claimNode provides the authoritative guard.
func (v *MaintenanceRequestValidator) checkNoDuplicateMR(ctx context.Context, nodeName, selfName string) error {
	if v.Client == nil {
		return nil
	}

	var list v1alpha1.MaintenanceRequestList
	if err := v.Client.List(ctx, &list); err != nil {
		mrLog.Error("Failed to list MaintenanceRequests; allowing request to avoid blocking on transient errors",
			"error", err)

		return nil
	}

	for i := range list.Items {
		mr := &list.Items[i]
		if mr.Name == selfName {
			continue
		}

		if mr.Spec != nil && mr.Spec.HealthEvent != nil && mr.Spec.HealthEvent.NodeName == nodeName {
			return fmt.Errorf("node %q already has an active MaintenanceRequest %q", nodeName, mr.Name)
		}
	}

	return nil
}

func (v *MaintenanceRequestValidator) checkNodeExists(ctx context.Context, nodeName string) error {
	if v.Client == nil {
		return nil
	}

	var node corev1.Node
	if err := v.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("spec.healthEvent.nodeName references non-existent node %q", nodeName)
		}

		mrLog.Error("Failed to look up node; allowing request to avoid blocking on transient errors",
			"error", err, "node", nodeName)
	}

	return nil
}

// checkUserFieldsImmutable verifies that the user-specified fields on a
// HealthEvent have not changed between old and new. The controller
// auto-populates id, version, generatedTimestamp, and metadata after
// creation, so those fields are deliberately excluded — comparing the
// full proto would reject the controller's own spec-persist Update.
func checkUserFieldsImmutable(old, new *pb.HealthEvent) error {
	if old == nil {
		return nil
	}

	if new == nil {
		return fmt.Errorf("spec.healthEvent is immutable after creation")
	}

	if err := checkScalarFieldsImmutable(old, new); err != nil {
		return err
	}

	return checkCompositeFieldsImmutable(old, new)
}

func checkScalarFieldsImmutable(old, new *pb.HealthEvent) error {
	checks := []struct {
		name    string
		changed bool
	}{
		{"agent", old.Agent != new.Agent},
		{"componentClass", old.ComponentClass != new.ComponentClass},
		{"checkName", old.CheckName != new.CheckName},
		{"nodeName", old.NodeName != new.NodeName},
		{"isFatal", old.IsFatal != new.IsFatal},
		{"isHealthy", old.IsHealthy != new.IsHealthy},
		{"recommendedAction", old.RecommendedAction != new.RecommendedAction},
		{"message", old.Message != new.Message},
		{"customRecommendedAction", old.CustomRecommendedAction != new.CustomRecommendedAction},
		{"processingStrategy", old.ProcessingStrategy != new.ProcessingStrategy},
	}

	for _, c := range checks {
		if c.changed {
			return fmt.Errorf("spec.healthEvent.%s is immutable after creation", c.name)
		}
	}

	return nil
}

func checkCompositeFieldsImmutable(old, new *pb.HealthEvent) error {
	if !slices.Equal(old.ErrorCode, new.ErrorCode) {
		return fmt.Errorf("spec.healthEvent.errorCode is immutable after creation")
	}

	if !overridesEqual(old.QuarantineOverrides, new.QuarantineOverrides) {
		return fmt.Errorf("spec.healthEvent.quarantineOverrides is immutable after creation")
	}

	if !overridesEqual(old.DrainOverrides, new.DrainOverrides) {
		return fmt.Errorf("spec.healthEvent.drainOverrides is immutable after creation")
	}

	if !entitiesEqual(old.EntitiesImpacted, new.EntitiesImpacted) {
		return fmt.Errorf("spec.healthEvent.entitiesImpacted is immutable after creation")
	}

	return nil
}

func overridesEqual(a, b *pb.BehaviourOverrides) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return proto.Equal(a, b)
}

func entitiesEqual(a, b []*pb.Entity) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}

	return true
}

func timestampsEqual(a, b *timestamppb.Timestamp) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return a.AsTime().Equal(b.AsTime())
}

func isStartTimeInFuture(ts *timestamppb.Timestamp) bool {
	if ts == nil {
		return true
	}

	return ts.AsTime().After(time.Now())
}
