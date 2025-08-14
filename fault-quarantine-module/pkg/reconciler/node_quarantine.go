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

package reconciler

import (
	"context"
	"fmt"
	"time"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/common"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/config"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
)

// other modules may also update the node, so we need to make sure that we retry on conflict
var customBackoff = wait.Backoff{
	Steps:    10,
	Duration: 10 * time.Millisecond,
	Factor:   1.5,
	Jitter:   0.1,
}

type FaultQuarantineClient struct {
	// client is the Kubernetes client
	clientset  kubernetes.Interface
	dryRunMode bool
}

func NewFaultQuarantineClient(kubeconfig string, dryRun bool) (*FaultQuarantineClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		if kubeconfig == "" {
			return nil, fmt.Errorf("kubeconfig is not set")
		}

		// build config from kubeconfig file
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("error creating Kubernetes config from kubeconfig: %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating clientset: %w", err)
	}

	client := &FaultQuarantineClient{
		clientset:  clientset,
		dryRunMode: dryRun,
	}

	return client, nil
}

func (c *FaultQuarantineClient) GetK8sClient() kubernetes.Interface {
	return c.clientset
}

// nolint: cyclop,gocognit //fix this as part of NGCC-21793
func (c *FaultQuarantineClient) TaintAndCordonNodeAndSetAnnotations(
	ctx context.Context,
	nodename string,
	taints []config.Taint,
	isCordon bool,
	annotations map[string]string,
	labels map[string]string,
) error {
	return retry.OnError(customBackoff, errors.IsConflict, func() error {
		node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodename, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}

		// Taints check
		if len(taints) > 0 {
			// map to track existing taints
			existingTaints := make(map[config.Taint]v1.Taint)
			for _, taint := range node.Spec.Taints {
				existingTaints[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] = taint
			}

			for _, taintConfig := range taints {
				key := config.Taint{Key: taintConfig.Key, Value: taintConfig.Value, Effect: string(taintConfig.Effect)}

				// Check if the taint is already present, if not then add it
				if _, exists := existingTaints[key]; !exists {
					klog.Infof("Tainting node %s with taint config: %+v", nodename, taintConfig)
					existingTaints[key] = v1.Taint{
						Key:    taintConfig.Key,
						Value:  taintConfig.Value,
						Effect: v1.TaintEffect(taintConfig.Effect),
					}
				}
			}

			node.Spec.Taints = []v1.Taint{}
			for _, taint := range existingTaints {
				node.Spec.Taints = append(node.Spec.Taints, taint)
			}
		}

		// Cordon check
		// nolint: cyclop, gocognit, nestif //fix this as part of NGCC-21793
		if isCordon {
			_, exist := node.Annotations[common.QuarantineHealthEventAnnotationKey]
			if node.Spec.Unschedulable {
				if exist {
					klog.Infof("Node %s already cordoned by FQM; skipping taint/annotation updates", nodename)
					return nil
				}

				klog.Infof("Node %s is cordoned manually; applying FQM taints/annotations", nodename)
			} else {
				// Cordoning the node since it is currently schedulable.
				klog.Infof("Cordoning node %s", nodename)

				if !c.dryRunMode {
					node.Spec.Unschedulable = true
				}
			}
		}

		// Annotation check
		if len(annotations) > 0 {
			if node.Annotations == nil {
				node.Annotations = make(map[string]string)
			}

			klog.Infof("Setting annotations %+v on node %s", annotations, nodename)
			// set annotations
			for annotationKey, annotationValue := range annotations {
				node.Annotations[annotationKey] = annotationValue
			}
		}

		// Labels check
		if len(labels) > 0 {
			klog.Infof("Adding labels on node %s", nodename)

			for k, v := range labels {
				node.Labels[k] = v
			}
		}

		_, err = c.clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})

		if err != nil {
			return fmt.Errorf("failed to taint node: %w", err)
		}

		return nil
	})
}

// nolint: cyclop,gocognit //fix this as part of NGCC-21793
func (c *FaultQuarantineClient) UnTaintAndUnCordonNodeAndRemoveAnnotations(
	ctx context.Context,
	nodename string,
	taints []config.Taint,
	isUnCordon bool,
	annotationKeys []string,
	labelsToRemove []string,
	labels map[string]string,
) error {
	return retry.OnError(customBackoff, errors.IsConflict, func() error {
		node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodename, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}

		// untaint check
		if len(taints) > 0 {
			taintsAlreadyPresentOnNodeMap := map[config.Taint]bool{}
			for _, taint := range node.Spec.Taints {
				taintsAlreadyPresentOnNodeMap[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] = true
			}

			// Check if the taints are present
			toRemove := map[config.Taint]bool{}

			for _, taintConfig := range taints {
				key := config.Taint{
					Key:    taintConfig.Key,
					Value:  taintConfig.Value,
					Effect: taintConfig.Effect,
				}

				found := taintsAlreadyPresentOnNodeMap[key]
				if !found {
					klog.Infof("Node %s already does not have the taint: %+v", nodename, taintConfig)
				} else {
					toRemove[taintConfig] = true
				}
			}

			if len(toRemove) == 0 {
				return nil
			}

			klog.Infof("Untainting node %s with taint config: %+v", nodename, toRemove)

			newTaints := []v1.Taint{}

			for _, taint := range node.Spec.Taints {
				if toRemove[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] {
					// Skip taints that need to be removed
					continue
				}

				newTaints = append(newTaints, taint)
			}

			node.Spec.Taints = newTaints
		}

		// uncordon check
		if isUnCordon {
			klog.Infof("Uncordoning node %s", nodename)

			if !c.dryRunMode {
				node.Spec.Unschedulable = false
			}

			// Only add labels if labels map is provided (non-nil and non-empty)
			if len(labels) > 0 {
				klog.Infof("Adding labels on node %s", nodename)

				for k, v := range labels {
					node.Labels[k] = v
				}

				uncordonReason := node.Labels[cordonedReasonLabelKey]

				if len(uncordonReason) > 55 {
					uncordonReason = uncordonReason[:55]
				}

				node.Labels[uncordonedReasonLabelkey] = uncordonReason + "-removed"
			}
		}

		// Annotation check
		if len(annotationKeys) > 0 && node.Annotations != nil {
			// remove annotations
			for _, annotationKey := range annotationKeys {
				klog.Infof("Removing annotation key %s from node %s", annotationKey, nodename)
				delete(node.Annotations, annotationKey)
			}
		}

		// Label check
		if len(labelsToRemove) > 0 {
			for _, labelKey := range labelsToRemove {
				klog.Infof("Removing label key %s from node %s", labelKey, nodename)
				delete(node.Labels, labelKey)
			}
		}

		_, err = c.clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to remove taint from node: %w", err)
		}

		return nil
	})
}

func (c *FaultQuarantineClient) GetNodeAnnotations(ctx context.Context, nodename string) (map[string]string, error) {
	node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodename, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node: %w", err)
	}

	if node.Annotations == nil {
		return map[string]string{}, nil
	}

	// return a copy of the annotations map to prevent unintended modifications
	annotations := make(map[string]string)
	for key, value := range node.Annotations {
		annotations[key] = value
	}

	return annotations, nil
}

func (c *FaultQuarantineClient) GetNodesWithAnnotation(ctx context.Context, annotationKey string) ([]string, error) {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var nodesWithAnnotation []string

	for _, node := range nodes.Items {
		annotationValue, exists := node.Annotations[annotationKey]
		if exists && annotationValue != "" {
			nodesWithAnnotation = append(nodesWithAnnotation, node.Name)
		}
	}

	return nodesWithAnnotation, nil
}
