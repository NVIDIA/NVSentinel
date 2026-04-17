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

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	memoryHogLabel = "nvsentinel.nvidia.com/memory-hog"
	pollInterval   = 10 * time.Second
)

var memoryreclaimGVR = schema.GroupVersionResource{
	Group:    "demo.nvsentinel.nvidia.com",
	Version:  "v1alpha1",
	Resource: "memoryreclaims",
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[memory-reclaim-controller] ")

	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "nvsentinel"
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to get in-cluster config: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create kubernetes clientset: %v", err)
	}

	log.Printf("Starting controller (namespace=%s, poll=%s)", namespace, pollInterval)

	ctx := context.Background()
	processed := make(map[string]bool)

	for {
		if err := reconcileLoop(ctx, dynClient, clientset, namespace, processed); err != nil {
			log.Printf("ERROR: reconcile loop: %v", err)
		}
		time.Sleep(pollInterval)
	}
}

func reconcileLoop(
	ctx context.Context,
	dynClient dynamic.Interface,
	clientset kubernetes.Interface,
	namespace string,
	processed map[string]bool,
) error {
	list, err := dynClient.Resource(memoryreclaimGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing MemoryReclaim CRs: %w", err)
	}

	for i := range list.Items {
		cr := &list.Items[i]
		name := cr.GetName()

		if processed[name] {
			continue
		}

		if isAlreadyCompleted(cr) {
			processed[name] = true
			continue
		}

		log.Printf("Processing MemoryReclaim %q", name)

		nodeName, found, err := unstructured.NestedString(cr.Object, "spec", "nodeName")
		if err != nil || !found {
			log.Printf("WARN: MemoryReclaim %q has no spec.nodeName, skipping", name)
			continue
		}

		deleted, err := deleteMemoryHogPods(ctx, clientset, namespace, nodeName)
		if err != nil {
			log.Printf("ERROR: deleting pods for node %q: %v", nodeName, err)
			continue
		}

		log.Printf("Deleted %d memory-hog pod(s) on node %q", deleted, nodeName)

		if err := updateCRStatus(ctx, dynClient, cr); err != nil {
			log.Printf("ERROR: updating status for MemoryReclaim %q: %v", name, err)
			continue
		}

		log.Printf("Marked MemoryReclaim %q as MemoryReclaimed=True", name)
		processed[name] = true
	}

	return nil
}

func isAlreadyCompleted(cr *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(cr.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == "MemoryReclaimed" && cond["status"] == "True" {
			return true
		}
	}

	return false
}

func deleteMemoryHogPods(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, nodeName string,
) (int, error) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=true", memoryHogLabel),
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return 0, fmt.Errorf("listing pods: %w", err)
	}

	deleted := 0
	for _, pod := range pods.Items {
		log.Printf("  Deleting pod %s/%s on node %s", pod.Namespace, pod.Name, nodeName)
		if err := clientset.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			log.Printf("  WARN: failed to delete pod %s: %v", pod.Name, err)
			continue
		}
		deleted++
	}

	return deleted, nil
}

func updateCRStatus(ctx context.Context, dynClient dynamic.Interface, cr *unstructured.Unstructured) error {
	now := time.Now().UTC().Format(time.RFC3339)

	newCondition := map[string]interface{}{
		"type":               "MemoryReclaimed",
		"status":             string(corev1.ConditionTrue),
		"reason":             "HogPodsDeleted",
		"message":            "Memory hog pods deleted, memory pressure resolved",
		"lastTransitionTime": now,
	}

	conditions, _, _ := unstructured.NestedSlice(cr.Object, "status", "conditions")

	updated := false
	for i, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == "MemoryReclaimed" {
			conditions[i] = newCondition
			updated = true
			break
		}
	}

	if !updated {
		conditions = append(conditions, newCondition)
	}

	if err := unstructured.SetNestedSlice(cr.Object, conditions, "status", "conditions"); err != nil {
		return fmt.Errorf("setting conditions: %w", err)
	}

	if err := unstructured.SetNestedField(cr.Object, now, "status", "completionTime"); err != nil {
		return fmt.Errorf("setting completionTime: %w", err)
	}

	_, err := dynClient.Resource(memoryreclaimGVR).UpdateStatus(ctx, cr, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating CR status: %w", err)
	}

	return nil
}
