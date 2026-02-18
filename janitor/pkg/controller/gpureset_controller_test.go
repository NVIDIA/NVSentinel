// Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	prmproto "github.com/prometheus/client_model/go"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/gpuservices"
	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

// TODO: we cannot use a real instance of NodeLock in these tests because of the existing locking functionality in the
// isReady method of this controller which prevents concurrent GPUResets on the same node. The locking functionality in
// NodeLock will take precedence over the existing logic in this controller. As a result, we should remove the locking
// functionality in isReady and remove the corresponding tests.
type mockNodeLock struct{}

func (m *mockNodeLock) LockNode(ctx context.Context, maintenanceObject client.Object, nodeName string) bool {
	return true
}

func (m *mockNodeLock) CheckUnlock(ctx context.Context, maintenanceObject client.Object, nodeName string) (retryUnlock bool) {
	return false
}

var _ = Describe("GPUReset Controller", func() {
	var reconciler *GPUResetReconciler
	var mgrClient client.Client
	ctx := context.Background()
	var cancel context.CancelFunc

	defaultBackOffLimit := int32(2)
	defaultActiveDeadlineSeconds := int64(180)
	defaultTTLSecondsAfterFinished := int32(86400)
	gpuOperatorServiceManager, err := gpuservices.NewManager("gpu-operator", gpuservices.ManagerSpec{})
	Expect(err).NotTo(HaveOccurred())
	privileged := true
	defaultSecurityContext := corev1.SecurityContext{Privileged: &privileged}
	defaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}

	BeforeEach(func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  k8sClient.Scheme(),
			Metrics: server.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())

		mgrClient = mgr.GetClient()

		Expect(mgr.GetFieldIndexer().IndexField(ctx, &v1alpha1.GPUReset{}, "spec.nodeName", func(obj client.Object) []string {
			gr := obj.(*v1alpha1.GPUReset)
			if gr.Spec.NodeName == "" {
				return nil
			}
			return []string{gr.Spec.NodeName}
		})).To(Succeed())

		janitorNamespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dgxc-janitor-system",
			},
		}
		err = mgrClient.Create(ctx, janitorNamespace)
		Expect(client.IgnoreAlreadyExists(err)).NotTo(HaveOccurred())

		testServiceManager := gpuOperatorServiceManager

		customTemplate := &batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Labels: map[string]string{
					"test.label": "true",
				},
				Annotations: map[string]string{
					"test.annotation": "true",
				},
			},
			Spec: batchv1.JobSpec{
				BackoffLimit:            &defaultBackOffLimit,
				ActiveDeadlineSeconds:   &defaultActiveDeadlineSeconds,
				TTLSecondsAfterFinished: &defaultTTLSecondsAfterFinished,
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyOnFailure,
						Containers: []corev1.Container{
							{
								Name:            "gpu-reset",
								Image:           "nvcr.io/nv-ngc-devops/gpu-reset:latest",
								SecurityContext: &defaultSecurityContext,
								Resources:       defaultResources,
							},
						},
					},
				},
			},
		}

		reconciler = &GPUResetReconciler{
			Client:       mgrClient,
			Scheme:       k8sClient.Scheme(),
			FieldIndexer: mgr.GetFieldIndexer(),
			Config: &config.GPUResetControllerConfig{
				ServiceManager:      testServiceManager,
				ResolvedJobTemplate: customTemplate,
			},
			NodeLock:       &mockNodeLock{},
			serviceManager: testServiceManager,
		}

		// Default simulated pod status check helpers
		reconciler.checkPodsTerminatedFn = func(ctx context.Context, nodeName string) (bool, error) {
			return true, nil
		}
		reconciler.checkPodsReadyFn = func(ctx context.Context, nodeName string) (bool, error) {
			return true, nil
		}

		var testCtx context.Context
		testCtx, cancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(testCtx)).To(Succeed())
		}()
	})

	AfterEach(func() {
		// reset metrics
		metrics.GPUResetRequestsTotal.Reset()
		metrics.GPUResetRequestsCompletedTotal.Reset()
		metrics.GPUResetDurationSeconds.Reset()
		metrics.GPUResetActiveRequests.Reset()
		metrics.GPUResetPendingRequests.Reset()
		metrics.GPUResetFailureReasonsTotal.Reset()

		cancel()
	})

	Context("Successful Workflow with GPU Service Manager", func() {
		var nodeName = "success-test-node"
		var resetName = "success-test-reset"
		var typeNamespacedName = types.NamespacedName{Name: resetName}
		var node *corev1.Node

		BeforeEach(func() {
			By("Creating a test node")
			node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: make(map[string]string)}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		AfterEach(func() {
			By("Cleaning up resources")
			if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			reset := &v1alpha1.GPUReset{ObjectMeta: metav1.ObjectMeta{Name: resetName}}
			if err := k8sClient.Delete(ctx, reset); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			var jobList batchv1.JobList
			Expect(k8sClient.List(ctx, &jobList)).To(Succeed())
			for _, job := range jobList.Items {
				if strings.HasPrefix(job.Name, resetName) {
					if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}
		})

		It("should reconcile a GPUReset through all states successfully", func() {
			By("Creating a new GPUReset resource")
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
					Selector: &v1alpha1.GPUSelector{
						UUIDs: []string{"GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Waiting for the status to be initialized")
			var updatedReset v1alpha1.GPUReset
			Eventually(func(g Gomega) {
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Ready))).NotTo(BeNil())
				g.Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetPending))
			}, "10s", "250ms").Should(Succeed())
			Expect(controllerutil.ContainsFinalizer(&updatedReset, gpuResetFinalizer)).To(BeTrue())

			By("Waiting for the reset to be scheduled")
			Eventually(func(g Gomega) {
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.Ready))).To(BeTrue())
				g.Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetPending))
			}, "10s", "250ms").Should(Succeed())
			Expect(meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Ready)).Reason).To(Equal(string(v1alpha1.ReasonReadyForReset)))

			By("Waiting for services to be torn down")
			Eventually(func(g Gomega) {
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ServicesTornDown))).To(BeTrue())
			}, "10s", "250ms").Should(Succeed())

			By("Waiting for the reset job to be created")
			Eventually(func(g Gomega) {
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.JobRef).NotTo(BeNil())
				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ServicesTornDown))).To(BeTrue())
				g.Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetInProgress))

				// wait for the job to exist
				jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
				var createdJob batchv1.Job
				g.Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())

				cond := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.ResetJobCreated))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(cond.Reason).To(Equal(string(v1alpha1.ReasonResetJobCreationSucceeded)))
			}, "10s", "250ms").Should(Succeed())
			Expect(meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.ResetJobCreated)).Reason).To(Equal(string(v1alpha1.ReasonResetJobCreationSucceeded)))

			By("Simulating the job succeeding")
			var createdJob batchv1.Job
			jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
			Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			createdJob.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, &createdJob)).To(Succeed())

			By("Verifying the Job's Pod template has configured labels")
			Expect(createdJob.ObjectMeta.Labels).To(HaveKeyWithValue("test.label", "true"))

			By("Verifying the Job's Pod template has configured annotations")
			Expect(createdJob.ObjectMeta.Annotations).To(HaveKeyWithValue("test.annotation", "true"))

			By("Verifying the Job's Pod template container has the correct env var")
			var targetContainer corev1.Container
			containerFound := false
			for _, c := range createdJob.Spec.Template.Spec.Containers {
				if c.Name == "gpu-reset" {
					targetContainer = c
					containerFound = true
					break
				}
			}
			Expect(containerFound).To(BeTrue(), "Expected to find container in job template")

			expectedEnvVar := corev1.EnvVar{
				Name:  "NVIDIA_GPU_RESETS",
				Value: "GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6",
			}
			Expect(targetContainer.Env).To(ContainElement(expectedEnvVar))

			By("Waiting for the job to be marked as completed")
			Eventually(func(g Gomega) {
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ResetJobCompleted))).To(BeTrue())
				g.Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetInProgress))
			}, "10s", "250ms").Should(Succeed())
			Expect(meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.ResetJobCompleted)).Reason).To(Equal(string(v1alpha1.ReasonResetJobSucceeded)))

			By("Simulating pods becoming ready for service restoration")
			reconciler.checkPodsReadyFn = func(ctx context.Context, nodeName string) (bool, error) {
				return true, nil
			}

			By("Waiting for services to be restored")
			Eventually(func(g Gomega) {
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ServicesRestored))).To(BeTrue())
				g.Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetInProgress))
			}, "10s", "250ms").Should(Succeed())
			Expect(meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.ServicesRestored)).Reason).To(Equal(string(v1alpha1.ReasonServiceRestoreSucceeded)))

			By("Waiting for the reset to be marked as complete")
			Eventually(func(g Gomega) {
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.CompletionTime).NotTo(BeNil())
				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.Complete))).To(BeTrue())
				g.Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetSucceeded))
			}, "10s", "250ms").Should(Succeed())
			Expect(meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Complete)).Reason).To(Equal(string(v1alpha1.ReasonGPUResetSucceeded)))
		})
	})

	Context("Successful Workflow without GPU Service Manager (no-op service teardown/restoration)", func() {
		var nodeName = "noop-test-node"
		var resetName = "noop-test-reset"
		var typeNamespacedName = types.NamespacedName{Name: resetName}
		var node *corev1.Node
		var originalManager gpuservices.Manager

		BeforeEach(func() {
			By("Creating a test node")
			node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: make(map[string]string)}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			originalManager = reconciler.Config.ServiceManager

			noopManager, err := gpuservices.NewManager("", gpuservices.ManagerSpec{})
			Expect(err).NotTo(HaveOccurred())

			reconciler.Config.ServiceManager = noopManager
			reconciler.serviceManager = noopManager
		})

		AfterEach(func() {
			reconciler.Config.ServiceManager = originalManager
			reconciler.serviceManager = originalManager

			By("Cleaning up resources")
			if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			reset := &v1alpha1.GPUReset{ObjectMeta: metav1.ObjectMeta{Name: resetName}}
			if err := k8sClient.Delete(ctx, reset); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			var jobList batchv1.JobList
			Expect(k8sClient.List(ctx, &jobList)).To(Succeed())
			for _, job := range jobList.Items {
				if strings.HasPrefix(job.Name, resetName) {
					if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}
		})

		It("should skip service handling and complete when no manager is specified", func() {
			By("Creating a new GPUReset without a services manager")
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
					// GPUServicesManagerName intentionally omitted
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Waiting for the reset to complete")
			var updatedReset v1alpha1.GPUReset
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())

				if updatedReset.Status.JobRef != nil && !meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ResetJobCompleted)) {
					var createdJob batchv1.Job
					jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
					if err := k8sClient.Get(ctx, jobKey, &createdJob); err == nil {
						By("Verifying the Job's Pod template has configured labels")
						g.Expect(createdJob.ObjectMeta.Labels).To(HaveKeyWithValue("test.label", "true"))
						By("Verifying the Job's Pod template has configured annotations")
						g.Expect(createdJob.ObjectMeta.Annotations).To(HaveKeyWithValue("test.annotation", "true"))

						By("Verifying the no-op Job's Pod template container has an empty env var")
						var targetContainer corev1.Container
						containerFound := false
						for _, c := range createdJob.Spec.Template.Spec.Containers {
							if c.Name == "gpu-reset" {
								targetContainer = c
								containerFound = true
								break
							}
						}
						g.Expect(containerFound).To(BeTrue(), "Expected to find container in job template")

						// Value is empty because no selector was provided for this test
						expectedEnvVar := corev1.EnvVar{Name: "NVIDIA_GPU_RESETS", Value: ""}
						g.Expect(targetContainer.Env).To(ContainElement(expectedEnvVar))
						createdJob.Status.Succeeded = 1
						g.Expect(k8sClient.Status().Update(ctx, &createdJob)).To(Succeed())
					}
				}

				g.Expect(updatedReset.Status.CompletionTime).NotTo(BeNil())
			}, "15s", "250ms").Should(Succeed())

			By("Verifying the final state and skipped conditions")
			Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())

			// Check that teardown was skipped
			condTeardown := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.ServicesTornDown))
			Expect(condTeardown).NotTo(BeNil())
			Expect(condTeardown.Status).To(Equal(metav1.ConditionTrue))
			Expect(condTeardown.Reason).To(Equal(string(v1alpha1.ReasonSkipped)))

			// Check that restore was skipped
			condRestore := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.ServicesRestored))
			Expect(condRestore).NotTo(BeNil())
			Expect(condRestore.Status).To(Equal(metav1.ConditionTrue))
			Expect(condRestore.Reason).To(Equal(string(v1alpha1.ReasonSkipped)))

			// Check that the overall reset succeeded
			Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.Complete))).To(BeTrue())
			Expect(meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Complete)).Reason).To(Equal(string(v1alpha1.ReasonGPUResetSucceeded)))
			Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetSucceeded))

			// Check that the node labels were NOT modified
			var finalNode corev1.Node
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), &finalNode)).To(Succeed())
			Expect(finalNode.Labels).To(BeEmpty())
		})
	})

	Context("RestartPolicy Enforcement", func() {
		var nodeName = "tmpl-enf-test-node"
		var resetName = "tmpl-enf-test-reset"
		var typeNamespacedName = types.NamespacedName{Name: resetName}
		var node *corev1.Node

		BeforeEach(func() {
			By("Creating a test node")
			node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: make(map[string]string)}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		AfterEach(func() {
			By("Cleaning up resources")
			if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			reset := &v1alpha1.GPUReset{ObjectMeta: metav1.ObjectMeta{Name: resetName}}
			if err := k8sClient.Delete(ctx, reset); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			var jobList batchv1.JobList
			Expect(k8sClient.List(ctx, &jobList)).To(Succeed())
			for _, job := range jobList.Items {
				if strings.HasPrefix(job.Name, resetName) {
					if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}
		})

		It("should set a RestartPolicy for the template if absent", func() {
			customTemplate := reconciler.Config.ResolvedJobTemplate.DeepCopy()

			By("Not setting the Job's Pod template RestartPolicy")
			customTemplate.Spec.Template.Spec.RestartPolicy = ""
			reconciler.Config.ResolvedJobTemplate = customTemplate

			By("Creating a new GPUReset resource")
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Waiting for the reset job to be created")
			var createdJob batchv1.Job
			Eventually(func(g Gomega) {
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.JobRef).NotTo(BeNil())

				// Use the JobRef to poll for the Job itself
				jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
				g.Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			}, "10s", "250ms").Should(Succeed())

			By("Verifying the Job's Pod template RestartPolicy is set")
			Expect(createdJob.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))
		})
	})

	Context("Job Creation", func() {
		var nodeName = "job-create-test-node"
		var resetName = "job-create-test-reset"
		var typeNamespacedName = types.NamespacedName{Name: resetName}
		var node *corev1.Node

		BeforeEach(func() {
			By("Creating a test node")
			node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: make(map[string]string)}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		AfterEach(func() {
			By("Cleaning up resources")
			if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			reset := &v1alpha1.GPUReset{ObjectMeta: metav1.ObjectMeta{Name: resetName}}
			if err := k8sClient.Delete(ctx, reset); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			var jobList batchv1.JobList
			Expect(k8sClient.List(ctx, &jobList)).To(Succeed())
			for _, job := range jobList.Items {
				if strings.HasPrefix(job.Name, resetName) {
					if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}
		})

		It("should NOT remove pre-existing env vars when adding GPU ID env var", func() {

			By("Configuring container with env var")
			customTemplate := reconciler.Config.ResolvedJobTemplate.DeepCopy()
			customTemplate.Spec.Template.Spec.Containers = []corev1.Container{
				{
					Name:            "gpu-reset",
					Image:           "nvcr.io/nv-ngc-devops/gpu-reset:latest",
					SecurityContext: &defaultSecurityContext,
					Resources:       defaultResources,
					Env:             []corev1.EnvVar{{Name: "PRE_EXISTING_ENV", Value: "foo"}},
				},
			}
			reconciler.Config.ResolvedJobTemplate = customTemplate

			By("Creating a new GPUReset resource")
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
					Selector: &v1alpha1.GPUSelector{
						UUIDs: []string{"GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Waiting for the reset job to be created")
			var updatedReset v1alpha1.GPUReset
			var createdJob batchv1.Job
			Eventually(func(g Gomega) {
				_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.JobRef).NotTo(BeNil())

				// wait for the job to exist
				jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
				g.Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			}, "10s", "250ms").Should(Succeed())

			Expect(createdJob.Spec.Template.Spec.Containers).To(HaveLen(1))
			targetContainer := createdJob.Spec.Template.Spec.Containers[0]

			By("Verifying the Job's Pod template container has NVIDIA_GPU_RESETS env var with GPU ID")
			expectedEnvVar := corev1.EnvVar{
				Name:  "NVIDIA_GPU_RESETS",
				Value: "GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6",
			}
			Expect(targetContainer.Env).To(ContainElement(expectedEnvVar))

			By("Verifying the Job's Pod template container still has pre-existing env vars")
			Expect(targetContainer.Env).To(ContainElement(corev1.EnvVar{Name: "PRE_EXISTING_ENV", Value: "foo"}))
		})
	})

	Context("Node Lock", func() {
		var nodeName = "node-lock-test-node"
		var node *corev1.Node

		BeforeEach(func() {
			node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: make(map[string]string)}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		AfterEach(func() {
			if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			var resets v1alpha1.GPUResetList
			Expect(k8sClient.List(ctx, &resets)).To(Succeed())
			for _, r := range resets.Items {
				if r.Spec.NodeName == nodeName {
					if err := k8sClient.Delete(ctx, &r); err != nil && !apierrors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}

			var jobList batchv1.JobList
			Expect(k8sClient.List(ctx, &jobList, client.InNamespace("default"))).To(Succeed())
			for _, job := range jobList.Items {
				if strings.HasPrefix(job.Name, "first-reset") || strings.HasPrefix(job.Name, "second-reset") {
					if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}
		})

		It("should keep second reset not-ready until the first one is complete", func() {
			By("Creating the first GPUReset")
			reset1 := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: "first-reset"},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
				},
			}
			Expect(k8sClient.Create(ctx, reset1)).To(Succeed())

			// Ensure a different creation timestamp
			time.Sleep(2 * time.Second)

			By("Creating the second GPUReset for the same node")
			reset2 := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: "second-reset"},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
				},
			}
			Expect(k8sClient.Create(ctx, reset2)).To(Succeed())

			By("Waiting for the first reset to be initialized, scheduled, and the job created")
			var updatedReset1 v1alpha1.GPUReset
			var createdJob batchv1.Job
			reset1Key := types.NamespacedName{Name: reset1.Name}
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: reset1Key})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, reset1Key, &updatedReset1)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(updatedReset1.Status.Conditions, string(v1alpha1.Ready))).To(BeTrue())
				g.Expect(updatedReset1.Status.JobRef).NotTo(BeNil())
				g.Expect(updatedReset1.Status.Phase).To(Equal(v1alpha1.ResetInProgress))

				// wait for job to exist
				job1Key := types.NamespacedName{Name: updatedReset1.Status.JobRef.Name, Namespace: updatedReset1.Status.JobRef.Namespace}
				g.Expect(k8sClient.Get(ctx, job1Key, &createdJob)).To(Succeed())
			}, "10s", "100ms").Should(Succeed())

			By("Verifying the second reset is not ready due to resource contention")
			var updatedReset2 v1alpha1.GPUReset
			reset2Key := types.NamespacedName{Name: reset2.Name}
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: reset2Key})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, reset2Key, &updatedReset2)).To(Succeed())

				cond := meta.FindStatusCondition(updatedReset2.Status.Conditions, string(v1alpha1.Ready))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(string(v1alpha1.ReasonResourceContention)))
				g.Expect(cond.Message).To(ContainSubstring("Waiting for first-reset to complete"))
				g.Expect(updatedReset2.Status.Phase).To(Equal(v1alpha1.ResetPending))
			}, "10s", "100ms").Should(Succeed())

			By("Simulating job success and full completion of the first reset")
			Expect(updatedReset1.Status.JobRef).NotTo(BeNil(), "Expected reset1 to have a JobRef before simulating completion")
			job1Key := types.NamespacedName{Name: updatedReset1.Status.JobRef.Name, Namespace: updatedReset1.Status.JobRef.Namespace}
			Expect(k8sClient.Get(ctx, job1Key, &createdJob)).To(Succeed())
			createdJob.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, &createdJob)).To(Succeed())

			reconciler.checkPodsReadyFn = func(ctx context.Context, nodeName string) (bool, error) {
				return true, nil
			}

			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: reset1Key})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, reset1Key, &updatedReset1)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(updatedReset1.Status.Conditions, string(v1alpha1.Complete))).To(BeTrue())
				g.Expect(updatedReset1.Status.Phase).To(Equal(v1alpha1.ResetSucceeded))
			}, "10s", "100ms").Should(Succeed(), "Timed out waiting for reset1 to reach Complete state")

			By("Waiting for the second reset to become scheduled after the first completed")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: reset2Key})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, reset2Key, &updatedReset2)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(updatedReset2.Status.Conditions, string(v1alpha1.Ready))).To(BeTrue())
				g.Expect(updatedReset2.Status.Phase).To(Equal(v1alpha1.ResetPending))
			}, "10s", "100ms").Should(Succeed())
		})
	})

	Context("Failure Scenarios", func() {
		var nodeName = "fail-test-node"
		var resetName = "fail-test-reset"
		var typeNamespacedName = types.NamespacedName{Name: resetName}
		var node *corev1.Node

		BeforeEach(func() {
			node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: make(map[string]string)}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
		})

		AfterEach(func() {
			if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			reset := &v1alpha1.GPUReset{ObjectMeta: metav1.ObjectMeta{Name: resetName}}
			if err := k8sClient.Delete(ctx, reset); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, typeNamespacedName, reset)
				if apierrors.IsNotFound(err) {
					return
				}
				g.Expect(err).NotTo(HaveOccurred())

				if controllerutil.ContainsFinalizer(reset, gpuResetFinalizer) {
					controllerutil.RemoveFinalizer(reset, gpuResetFinalizer)
					g.Expect(k8sClient.Update(ctx, reset)).To(Succeed())
				}
			}, "20s", "250ms").Should(Succeed())

			var jobList batchv1.JobList
			Expect(k8sClient.List(ctx, &jobList, client.InNamespace("default"))).To(Succeed())
			for _, job := range jobList.Items {
				if strings.HasPrefix(job.Name, resetName) {
					if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}
		})

		It("should move to a Failed state if node is not found", func() {
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: "non-existent-node",
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.CompletionTime).NotTo(BeNil())
			}, "10s", "100ms").Should(Succeed())

			var updatedReset v1alpha1.GPUReset
			Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.Complete))).To(BeTrue())
			Expect(meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Complete)).Reason).To(Equal(string(v1alpha1.NodeNotFound)))
			Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetFailed))
		})

		It("should move to Failed state if service teardown times out", func() {
			// Override the default timeout for this specific test
			reconciler.Config.ServiceManager.Spec.TeardownTimeout = 100 * time.Millisecond
			reconciler.serviceManager = reconciler.Config.ServiceManager

			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
					Selector: &v1alpha1.GPUSelector{
						UUIDs: []string{"GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			// Ensure pods are "never" terminated
			reconciler.checkPodsTerminatedFn = func(ctx context.Context, nodeName string) (bool, error) {
				return false, nil
			}

			By("Waiting for workflow to enter terminal failure state due to teardown timeout")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var finalReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &finalReset)).To(Succeed())
				g.Expect(finalReset.Status.CompletionTime).NotTo(BeNil())
			}, "10s", "200ms").Should(Succeed())

			By("Verifying final state")
			var updatedReset v1alpha1.GPUReset
			Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.Complete))).To(BeTrue())
			failedCond := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Complete))
			Expect(failedCond.Reason).To(Equal(string(v1alpha1.ReasonServiceTeardownTimeoutExceeded)))
			Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetFailed))
		})

		It("should move to Failed state if service restoration times out", func() {
			// Override the default timeout for this specific test
			reconciler.Config.ServiceManager.Spec.RestoreTimeout = 100 * time.Millisecond
			reconciler.serviceManager = reconciler.Config.ServiceManager

			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
					Selector: &v1alpha1.GPUSelector{
						UUIDs: []string{"GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Waiting for the job to be finished successfully")
			var updatedReset v1alpha1.GPUReset
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())

				if updatedReset.Status.JobRef != nil && !meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ResetJobCompleted)) {
					var createdJob batchv1.Job
					jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
					g.Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
					createdJob.Status.Succeeded = 1
					g.Expect(k8sClient.Status().Update(ctx, &createdJob)).To(Succeed())
				}

				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ResetJobCompleted))).To(BeTrue())
			}, "10s", "100ms").Should(Succeed())

			// Ensure pods are "never" ready
			reconciler.checkPodsReadyFn = func(ctx context.Context, nodeName string) (bool, error) {
				return false, nil
			}

			By("Waiting for service restoration to begin")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				cond := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.ServicesRestored))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Reason).To(Equal(string(v1alpha1.ReasonRestoringServices)))
			}, "10s", "100ms").Should(Succeed())

			By("Simulating timeout by updating the LastTransitionTime")
			// Override timeout from the reconciler's config
			overriddenTimeout := reconciler.Config.ServiceManager.Spec.RestoreTimeout
			transitionTime := metav1.NewTime(time.Now().Add(-(overriddenTimeout + 50*time.Millisecond)))
			meta.SetStatusCondition(&updatedReset.Status.Conditions, metav1.Condition{
				Type:               string(v1alpha1.ServicesRestored),
				Status:             metav1.ConditionFalse,
				Reason:             string(v1alpha1.ReasonRestoringServices),
				Message:            "Waiting for services to be restored.",
				LastTransitionTime: transitionTime,
			})
			Expect(k8sClient.Status().Update(ctx, &updatedReset)).To(Succeed())

			By("Waiting for workflow to enter terminal failure state")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var finalReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &finalReset)).To(Succeed())
				g.Expect(finalReset.Status.CompletionTime).NotTo(BeNil())
			}, "10s", "100ms").Should(Succeed())

			By("Verifying final state")
			Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.Complete))).To(BeTrue())
			failedCond := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Complete))
			Expect(failedCond.Reason).To(Equal(string(v1alpha1.ReasonRestoreTimeoutExceeded)))
			Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetFailed))
		})

		It("should move to Failed state if the reset job is deleted before finished", func() {
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
					Selector: &v1alpha1.GPUSelector{
						UUIDs: []string{"GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Waiting for the job to be created")
			var updatedReset v1alpha1.GPUReset
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.JobRef).NotTo(BeNil())

				// wait for job to exist
				jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
				var createdJob batchv1.Job
				g.Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			}, "10s", "100ms").Should(Succeed())

			By("Manually deleting the job")
			jobToDelete := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      updatedReset.Status.JobRef.Name,
					Namespace: updatedReset.Status.JobRef.Namespace,
				},
			}
			Expect(k8sClient.Delete(ctx, jobToDelete, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())

			By("Waiting for workflow to enter terminal failure state")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var finalReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &finalReset)).To(Succeed())
				g.Expect(finalReset.Status.CompletionTime).NotTo(BeNil())
			}, "10s", "100ms").Should(Succeed())

			By("Verifying final state")
			Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.Complete))).To(BeTrue())
			failedCond := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Complete))
			Expect(failedCond.Reason).To(Equal(string(v1alpha1.ReasonResetJobNotFound)))
			Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetFailed))
		})

		It("should restore services and move to a Failed state if job fails", func() {
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
					Selector: &v1alpha1.GPUSelector{
						UUIDs: []string{"GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Waiting for the job to be created")
			var updatedReset v1alpha1.GPUReset
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.JobRef).NotTo(BeNil())

				// wait for job to exist
				jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
				var createdJob batchv1.Job
				g.Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			}, "10s", "100ms").Should(Succeed())

			By("Simulating job failure")
			var createdJob batchv1.Job
			jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
			Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			createdJob.Status.Failed = 1
			Expect(k8sClient.Status().Update(ctx, &createdJob)).To(Succeed())

			reconciler.checkPodsReadyFn = func(ctx context.Context, nodeName string) (bool, error) {
				return true, nil
			}

			By("Waiting for workflow to complete")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var finalReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &finalReset)).To(Succeed())
				g.Expect(finalReset.Status.CompletionTime).NotTo(BeNil())
			}, "10s", "100ms").Should(Succeed())

			By("Verifying final state")
			Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ServicesRestored))).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.Complete))).To(BeTrue())
			Expect(meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Complete)).Reason).To(Equal(string(v1alpha1.ReasonResetJobFailed)))
			Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetFailed))
		})
	})

	Context("Idempotency and Resilience", func() {
		var nodeName = "resilience-test-node"
		var resetName = "resilience-test-reset"
		var typeNamespacedName = types.NamespacedName{Name: resetName}
		var node *corev1.Node

		BeforeEach(func() {
			node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: make(map[string]string)}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			reconciler.checkPodsTerminatedFn = func(ctx context.Context, nodeName string) (bool, error) {
				return true, nil
			}
		})

		AfterEach(func() {
			if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			reset := &v1alpha1.GPUReset{ObjectMeta: metav1.ObjectMeta{Name: resetName}}
			if err := k8sClient.Delete(ctx, reset); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, typeNamespacedName, reset)
				if apierrors.IsNotFound(err) {
					return
				}
				g.Expect(err).NotTo(HaveOccurred())

				if controllerutil.ContainsFinalizer(reset, gpuResetFinalizer) {
					controllerutil.RemoveFinalizer(reset, gpuResetFinalizer)
					g.Expect(k8sClient.Update(ctx, reset)).To(Succeed())
				}
			}, "20s", "250ms").Should(Succeed())

			var jobList batchv1.JobList
			Expect(k8sClient.List(ctx, &jobList, client.InNamespace("default"))).To(Succeed())
			for _, job := range jobList.Items {
				if strings.HasPrefix(job.Name, resetName) {
					if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}
		})

		It("should detect pod state drift and re-initiate teardown", func() {
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
					Selector: &v1alpha1.GPUSelector{
						UUIDs: []string{"GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Reconciling until services teardown has succeeded")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ServicesTornDown))).To(BeTrue())
			}, "10s", "100ms").Should(Succeed())

			By("Verifying node labels are set to disabled")
			var updatedNode corev1.Node
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &updatedNode)).To(Succeed())
			Expect(updatedNode.Labels["nvidia.com/gpu.deploy.device-plugin"]).To(Equal("false"))

			By("Simulating an external change by reverting a node label")
			patch := client.MergeFrom(updatedNode.DeepCopy())
			if updatedNode.Labels == nil {
				updatedNode.Labels = make(map[string]string)
			}
			updatedNode.Labels["nvidia.com/gpu.deploy.device-plugin"] = "true"
			Expect(k8sClient.Patch(ctx, &updatedNode, patch)).To(Succeed())

			By("Simulating pod drift (e.g., pods restarted due to node label being reverted)")
			reconciler.checkPodsTerminatedFn = func(ctx context.Context, nodeName string) (bool, error) {
				return false, nil
			}

			By("Running reconciliation, expecting it to detect pod drift and reset the condition")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				cond := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.ServicesTornDown))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(string(v1alpha1.ReasonTearingDownServices)))
				g.Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetInProgress))
			}, "10s", "100ms").Should(Succeed())

			By("Running reconciliation again to enforce the correct state")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the node label has been reverted to back false")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &updatedNode)).To(Succeed())
			Expect(updatedNode.Labels["nvidia.com/gpu.deploy.device-plugin"]).To(Equal("false"))
		})
	})

	Context("Finalizer", func() {
		var nodeName = "finalizer-test-node"
		var resetName = "finalizer-test-reset"
		var typeNamespacedName = types.NamespacedName{Name: resetName}
		var node *corev1.Node
		var originalManager gpuservices.Manager

		BeforeEach(func() {
			node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: make(map[string]string)}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			originalManager = reconciler.Config.ServiceManager
		})

		AfterEach(func() {
			reconciler.Config.ServiceManager = originalManager
			reconciler.serviceManager = originalManager

			if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("should short-circuit and remove finalizer immediately if no services are configured", func() {
			By("Configuring the reconciler for a no-op services manager")
			noopManager, err := gpuservices.NewManager("", gpuservices.ManagerSpec{})
			Expect(err).NotTo(HaveOccurred())
			reconciler.Config.ServiceManager = noopManager
			reconciler.serviceManager = noopManager

			By("Pre-labeling node to simulate services being active")
			nodeKey := client.ObjectKeyFromObject(node)
			var prelabeledNode corev1.Node
			Expect(k8sClient.Get(ctx, nodeKey, &prelabeledNode)).To(Succeed())
			if prelabeledNode.Labels == nil {
				prelabeledNode.Labels = make(map[string]string)
			}
			patch := client.MergeFrom(prelabeledNode.DeepCopy())
			prelabeledNode.Labels["nvidia.com/gpu.deploy.device-plugin"] = "true"
			Expect(k8sClient.Patch(ctx, &prelabeledNode, patch)).To(Succeed())

			By("Creating the GPUReset resource")
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			var updatedReset v1alpha1.GPUReset
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(controllerutil.ContainsFinalizer(&updatedReset, gpuResetFinalizer)).To(BeTrue())
			}, "5s", "100ms").Should(Succeed())

			By("Deleting the GPUReset resource")
			Expect(k8sClient.Delete(ctx, reset)).To(Succeed())

			By("Waiting for the resource to be immediately deleted (finalizer short-circuited)")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())

				var deletedReset v1alpha1.GPUReset
				err = k8sClient.Get(ctx, typeNamespacedName, &deletedReset)
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}, "5s", "100ms").Should(Succeed())

			By("Verifying the node label was NEVER touched")
			var finalNode corev1.Node
			Expect(k8sClient.Get(ctx, nodeKey, &finalNode)).To(Succeed())
			Expect(finalNode.Labels["nvidia.com/gpu.deploy.device-plugin"]).To(Equal("true"))
		})

		It("should restore managed services when deleted mid-workflow", func() {
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec: v1alpha1.GPUResetSpec{
					NodeName: nodeName,
				},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Reconciling until services are torn down")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())

				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.ServicesTornDown))).To(BeTrue())
			}, "10s", "250ms").Should(Succeed())

			By("Verifying services are disabled")
			var updatedNode corev1.Node
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &updatedNode)).To(Succeed())
			Expect(updatedNode.Labels["nvidia.com/gpu.deploy.device-plugin"]).To(Equal("false"))

			By("Deleting the GPUReset resource mid-workflow")
			Expect(k8sClient.Delete(ctx, reset)).To(Succeed())

			By("Waiting for the Terminating condition to be set")
			var updatedReset v1alpha1.GPUReset
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.DeletionTimestamp).NotTo(BeNil())
				g.Expect(meta.IsStatusConditionTrue(updatedReset.Status.Conditions, string(v1alpha1.Terminating))).To(BeTrue())
				g.Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetTerminating))
			}, "10s", "250ms").Should(Succeed())

			By("Simulating service restoration success")
			reconciler.checkPodsReadyFn = func(ctx context.Context, nodeName string) (bool, error) {
				return true, nil
			}

			By("Reconciling until the resource is fully deleted")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())

				var deletedReset v1alpha1.GPUReset
				err = k8sClient.Get(ctx, typeNamespacedName, &deletedReset)
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}, "10s", "250ms").Should(Succeed())

			By("Verifying the services are re-enabled")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &updatedNode)).To(Succeed())
			Expect(updatedNode.Labels["nvidia.com/gpu.deploy.device-plugin"]).To(Equal("true"))
		})
	})

	Context("Metrics", func() {
		var nodeName string
		var resetName string
		var typeNamespacedName types.NamespacedName
		var node *corev1.Node

		BeforeEach(func() {
			nodeName = "metrics-test-node"
			resetName = "metrics-test-reset"
			typeNamespacedName = types.NamespacedName{Name: resetName}

			By("Creating a test node for metrics")
			node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: make(map[string]string)}}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())

			// Mock pod checks
			reconciler.checkPodsTerminatedFn = func(ctx context.Context, nodeName string) (bool, error) {
				return true, nil
			}
			reconciler.checkPodsReadyFn = func(ctx context.Context, nodeName string) (bool, error) {
				return true, nil
			}
		})

		AfterEach(func() {
			if err := k8sClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			reset := &v1alpha1.GPUReset{ObjectMeta: metav1.ObjectMeta{Name: resetName}}
			if err := k8sClient.Delete(ctx, reset); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, typeNamespacedName, reset)
				if apierrors.IsNotFound(err) {
					return
				}
				g.Expect(err).NotTo(HaveOccurred())

				if controllerutil.ContainsFinalizer(reset, gpuResetFinalizer) {
					controllerutil.RemoveFinalizer(reset, gpuResetFinalizer)
					g.Expect(k8sClient.Update(ctx, reset)).To(Succeed())
				}
			}, "20s", "250ms").Should(Succeed())

			var jobList batchv1.JobList
			Expect(k8sClient.List(ctx, &jobList, client.InNamespace("default"))).To(Succeed())
			for _, job := range jobList.Items {
				if strings.HasPrefix(job.Name, resetName) {
					if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}
		})

		It("should correctly increment lifecycle metrics for a successful reset", func() {
			By("Creating a new GPUReset resource")
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec:       v1alpha1.GPUResetSpec{NodeName: nodeName},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Waiting for initialization (Pending)")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.StartTime).NotTo(BeNil())
			}, "10s", "250ms").Should(Succeed())

			Expect(testutil.ToFloat64(metrics.GPUResetRequestsTotal.WithLabelValues(nodeName))).To(Equal(1.0))
			Expect(testutil.ToFloat64(metrics.GPUResetPendingRequests.WithLabelValues(nodeName))).To(Equal(0.0))
			Expect(testutil.ToFloat64(metrics.GPUResetActiveRequests.WithLabelValues(nodeName))).To(Equal(0.0))

			By("Waiting for promotion to Active")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				cond := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Ready))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}, "10s", "250ms").Should(Succeed())

			Expect(testutil.ToFloat64(metrics.GPUResetPendingRequests.WithLabelValues(nodeName))).To(Equal(0.0))
			Expect(testutil.ToFloat64(metrics.GPUResetActiveRequests.WithLabelValues(nodeName))).To(Equal(1.0))

			By("Waiting for the job to be created")
			var updatedReset v1alpha1.GPUReset
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.JobRef).NotTo(BeNil())

				// wait for job to exist
				jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
				var createdJob batchv1.Job
				g.Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			}, "10s", "100ms").Should(Succeed())

			By("Simulating the job succeeding")
			var createdJob batchv1.Job
			jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
			Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			createdJob.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, &createdJob)).To(Succeed())

			By("Waiting for final completion (Success)")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())

				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())

				g.Expect(updatedReset.Status.CompletionTime).NotTo(BeNil())
				g.Expect(updatedReset.Status.Phase).To(Equal(v1alpha1.ResetSucceeded))
			}, "10s", "250ms").Should(Succeed())
			By("Waiting for final completion (Success)")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.CompletionTime).NotTo(BeNil())
			}, "10s", "250ms").Should(Succeed())

			Expect(testutil.ToFloat64(metrics.GPUResetActiveRequests.WithLabelValues(nodeName))).To(Equal(0.0))
			Expect(testutil.ToFloat64(metrics.GPUResetPendingRequests.WithLabelValues(nodeName))).To(Equal(0.0))
			Expect(testutil.ToFloat64(metrics.GPUResetRequestsCompletedTotal.WithLabelValues(nodeName, "success"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(metrics.GPUResetRequestsCompletedTotal.WithLabelValues(nodeName, "failure"))).To(Equal(0.0))

			By("Checking the duration histogram for success")
			metricObsr, err := metrics.GPUResetDurationSeconds.GetMetricWithLabelValues(nodeName, "success")
			Expect(err).NotTo(HaveOccurred())

			metric := metricObsr.(prometheus.Metric)
			var dto prmproto.Metric
			Expect(metric.Write(&dto)).To(Succeed())

			histogram := dto.GetHistogram()
			Expect(histogram).NotTo(BeNil())
			Expect(histogram.GetSampleCount()).To(Equal(uint64(1)))
			Expect(histogram.GetSampleSum()).To(BeNumerically(">", 0))
		})

		It("should correctly increment failure metrics for a failed reset", func() {
			By("Creating a new GPUReset resource")
			reset := &v1alpha1.GPUReset{
				ObjectMeta: metav1.ObjectMeta{Name: resetName},
				Spec:       v1alpha1.GPUResetSpec{NodeName: nodeName},
			}
			Expect(k8sClient.Create(ctx, reset)).To(Succeed())

			By("Waiting for initialization (Pending)")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.StartTime).NotTo(BeNil())
			}, "10s", "250ms").Should(Succeed())

			Expect(testutil.ToFloat64(metrics.GPUResetRequestsTotal.WithLabelValues(nodeName))).To(Equal(1.0))
			Expect(testutil.ToFloat64(metrics.GPUResetPendingRequests.WithLabelValues(nodeName))).To(Equal(0.0))
			Expect(testutil.ToFloat64(metrics.GPUResetActiveRequests.WithLabelValues(nodeName))).To(Equal(0.0))

			By("Waiting for promotion to Active")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var updatedReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				cond := meta.FindStatusCondition(updatedReset.Status.Conditions, string(v1alpha1.Ready))
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}, "10s", "250ms").Should(Succeed())

			Expect(testutil.ToFloat64(metrics.GPUResetPendingRequests.WithLabelValues(nodeName))).To(Equal(0.0))
			Expect(testutil.ToFloat64(metrics.GPUResetActiveRequests.WithLabelValues(nodeName))).To(Equal(1.0))

			By("Waiting for the job to be created")
			var updatedReset v1alpha1.GPUReset
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &updatedReset)).To(Succeed())
				g.Expect(updatedReset.Status.JobRef).NotTo(BeNil())

				// wait for job to exist
				jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
				var createdJob batchv1.Job
				g.Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			}, "10s", "100ms").Should(Succeed())

			By("Simulating the job failing")
			var createdJob batchv1.Job
			jobKey := types.NamespacedName{Name: updatedReset.Status.JobRef.Name, Namespace: updatedReset.Status.JobRef.Namespace}
			Expect(k8sClient.Get(ctx, jobKey, &createdJob)).To(Succeed())
			createdJob.Status.Failed = 1
			Expect(k8sClient.Status().Update(ctx, &createdJob)).To(Succeed())

			By("Waiting for workflow to complete")
			Eventually(func(g Gomega) {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var finalReset v1alpha1.GPUReset
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &finalReset)).To(Succeed())
				g.Expect(finalReset.Status.CompletionTime).NotTo(BeNil())
			}, "10s", "100ms").Should(Succeed())

			Expect(testutil.ToFloat64(metrics.GPUResetActiveRequests.WithLabelValues(nodeName))).To(Equal(0.0))
			Expect(testutil.ToFloat64(metrics.GPUResetPendingRequests.WithLabelValues(nodeName))).To(Equal(0.0))
			Expect(testutil.ToFloat64(metrics.GPUResetRequestsCompletedTotal.WithLabelValues(nodeName, "failure"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(metrics.GPUResetRequestsCompletedTotal.WithLabelValues(nodeName, "success"))).To(Equal(0.0))
			Expect(testutil.ToFloat64(metrics.GPUResetFailureReasonsTotal.WithLabelValues(nodeName, string(v1alpha1.ReasonResetJobFailed)))).To(Equal(1.0))

			By("Checking the duration histogram for failure")
			metricObsr, err := metrics.GPUResetDurationSeconds.GetMetricWithLabelValues(nodeName, "failure")
			Expect(err).NotTo(HaveOccurred())

			metric := metricObsr.(prometheus.Metric)
			var dto prmproto.Metric
			Expect(metric.Write(&dto)).To(Succeed())

			histogram := dto.GetHistogram()
			Expect(histogram).NotTo(BeNil())
			Expect(histogram.GetSampleCount()).To(Equal(uint64(1)))
			Expect(histogram.GetSampleSum()).To(BeNumerically(">", 0))
		})
	})
})
