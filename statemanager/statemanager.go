// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package statemanager

import (
	"context"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	NVSentinelStateLabelKey = "dgxc.nvidia.com/nvsentinel-state"
)

type NVSentinelStateLabelValue string

const (
	// Label values applied by the fault-quarantine-module:
	QuarantinedLabelValue NVSentinelStateLabelValue = "quarantined"

	// Label values applied by the node-drainer-module:
	DrainingLabelValue       NVSentinelStateLabelValue = "draining"
	DrainSucceededLabelValue NVSentinelStateLabelValue = "drain-succeeded"
	DrainFailedLabelValue    NVSentinelStateLabelValue = "drain-failed"

	// Label values applied by the fault-remediation-module:
	RemediatingLabelValue          NVSentinelStateLabelValue = "remediating"
	RemediationSucceededLabelValue NVSentinelStateLabelValue = "remediation-succeeded"
	RemediationFailedLabelValue    NVSentinelStateLabelValue = "remediation-failed"
)

/*
The StateManager interface is leveraged by both the node-drainer-module and the fault-remediation-module to manage the
lifecycle of the dgxc.nvidia.com/nvsentinel-state node label. Note that the fault-quarantine-module relies on its
existing node object update calls to add and remove this label.

Example label sequences:
1. Successful node remediation: quarantined, draining, drain-succeeded, remediating, remediation-succeeded, label removed
2. Failed node remediation: quarantined, draining, drain-succeeded, remediating, remediation-failed
3. Failed draining: quarantined, draining, drain-failed
4. Drain canceled by a healthy event: quarantined, draining, drain-succeeded

Note that the fault-remediation-module is responsible for also removing the label when a health event results in the
node being uncordoned. Additionally, we do not have a drain-canceled state because if an in-progress drain was canceled,
the fault-quarantine-module would've removed this label completely in response to the healthy HealthEvent.
*/
type StateManager interface {
	UpdateNVSentinelStateNodeLabel(ctx context.Context, nodeName string,
		newStateLabelValue NVSentinelStateLabelValue, removeStateLabel bool) (bool, error)
}

type stateManager struct {
	clientSet kubernetes.Interface
}

func NewStateManager(clientSet kubernetes.Interface) StateManager {
	return &stateManager{
		clientSet: clientSet,
	}
}

// UpdateNVSentinelStateNodeLabel will update the given node to the given value for the dgxc.nvidia.com/nvsentinel-state
// label or it will remove the given label if removeStateLabel is true.
func (manager *stateManager) UpdateNVSentinelStateNodeLabel(ctx context.Context, nodeName string,
	newStateLabelValue NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
	nodeModified := false
	err := retry.OnError(retry.DefaultRetry, errors.IsConflict, func() error {
		node, err := manager.clientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		currentValue, exists := node.Labels[NVSentinelStateLabelKey]
		if removeStateLabel {
			if !exists {
				klog.Infof("Label %s is already absent for node %s", NVSentinelStateLabelKey, nodeName)
				return nil
			}
			delete(node.Labels, NVSentinelStateLabelKey)
		} else {
			klog.Infof("Setting %s label to %s for node %s", NVSentinelStateLabelKey, newStateLabelValue, nodeName)
			if exists && currentValue == string(newStateLabelValue) {
				klog.Infof("Label %s with value %s is already set for node %s", NVSentinelStateLabelKey,
					newStateLabelValue, nodeName)
				return nil
			}
			node.Labels[NVSentinelStateLabelKey] = string(newStateLabelValue)
		}
		_, err = manager.clientSet.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		nodeModified = true
		klog.Infof("Label %s updated successfully for node %s", NVSentinelStateLabelKey, nodeName)
		return nil
	})
	return nodeModified, err
}
