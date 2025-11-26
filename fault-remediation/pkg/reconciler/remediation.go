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

package reconciler

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"text/template"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/crstatus"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

const (
	// Environment variable names
	LogCollectorManifestPathEnv = "LOG_COLLECTOR_MANIFEST_PATH"
)

type FaultRemediationClient struct {
	clientset         dynamic.Interface
	kubeClient        kubernetes.Interface
	restMapper        *restmapper.DeferredDiscoveryRESTMapper
	dryRunMode        []string
	template          *template.Template
	templateData      TemplateData
	annotationManager NodeAnnotationManagerInterface
	statusChecker     *crstatus.CRStatusChecker
	// nodeExistsFunc allows tests to override node existence checking.
	// If nil, uses the default implementation that checks with kubeClient.
	nodeExistsFunc func(ctx context.Context, nodeName string) (*corev1.Node, error)
}

// TemplateData holds the data to be inserted into the template
type TemplateData struct {
	NodeName          string
	HealthEventID     string
	RecommendedAction protos.RecommendedAction
	TemplateMountPath string
	TemplateFileName  string
	config.MaintenanceResource
}

// nolint: cyclop // todo
func NewK8sClient(kubeconfig string, dryRun bool, templateData TemplateData) (*FaultRemediationClient,
	kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		if kubeconfig == "" {
			return nil, nil, fmt.Errorf("kubeconfig is not set")
		}

		// build config from kubeconfig file
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, nil, fmt.Errorf("error creating Kubernetes config from kubeconfig: %w", err)
		}
	}

	clientset, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating clientset: %w", err)
	}

	// Create typed Kubernetes client for Jobs
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating kubernetes client: %w", err)
	}

	// Create discovery client for RESTMapper
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating discovery client: %w", err)
	}

	// Create RESTMapper for GVK to GVR conversion
	cachedClient := memory.NewMemCacheClient(discoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)

	// Construct full template path
	templatePath := filepath.Join(templateData.TemplateMountPath, templateData.TemplateFileName)

	// Check if the template file exists
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("template file does not exist: %s", templatePath)
	}

	// Read and parse the template
	templateContent, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, nil, fmt.Errorf("error reading template file: %w", err)
	}

	tmpl := template.New("maintenance")

	tmpl, err = tmpl.Parse(string(templateContent))
	if err != nil {
		return nil, nil, fmt.Errorf("error parsing template: %w", err)
	}

	client := &FaultRemediationClient{
		clientset:    clientset,
		kubeClient:   kubeClient,
		restMapper:   mapper,
		template:     tmpl,
		templateData: templateData,
	}

	if dryRun {
		client.dryRunMode = []string{metav1.DryRunAll}
	} else {
		client.dryRunMode = []string{}
	}

	// Initialize annotation manager
	client.annotationManager = NewNodeAnnotationManager(kubeClient)

	client.statusChecker = crstatus.NewCRStatusChecker(
		clientset,
		mapper,
		&templateData.MaintenanceResource,
		dryRun,
	)

	return client, kubeClient, nil
}

func (c *FaultRemediationClient) GetAnnotationManager() NodeAnnotationManagerInterface {
	return c.annotationManager
}

func (c *FaultRemediationClient) GetStatusChecker() *crstatus.CRStatusChecker {
	return c.statusChecker
}

func (c *FaultRemediationClient) CreateMaintenanceResource(
	ctx context.Context,
	healthEventData *HealthEventData,
) (bool, string) {
	healthEvent := healthEventData.HealthEvent
	healthEventID := healthEventData.ID

	// Generate CR name
	crName := fmt.Sprintf("maintenance-%s-%s", healthEvent.NodeName, healthEventID)

	// Skip custom resource creation if dry-run is enabled
	if len(c.dryRunMode) > 0 {
		log.Printf("DRY-RUN: Skipping custom resource creation for node %s", healthEvent.NodeName)
		return true, crName
	}

	// Get the node object to extract UID for owner reference
	// This also verifies the node exists before creating CR
	node, err := c.getNodeForOwnerReference(ctx, healthEvent.NodeName)
	if err != nil {
		slog.Warn("Failed to get node for owner reference, skipping CR creation",
			"node", healthEvent.NodeName,
			"error", err)

		return false, ""
	}

	log.Printf("Creating RebootNode CR for node: %s (UID: %s)", healthEvent.NodeName, node.UID)
	c.templateData.NodeName = healthEvent.NodeName
	c.templateData.RecommendedAction = healthEvent.RecommendedAction
	c.templateData.HealthEventID = healthEventID

	// Execute the template
	var buf bytes.Buffer
	if err := c.template.Execute(&buf, c.templateData); err != nil {
		slog.Error("Failed to execute maintenance template", "error", err)
		return false, ""
	}

	log.Printf("Generated YAML: %s", buf.String())

	// Convert YAML to unstructured
	var obj map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &obj); err != nil {
		slog.Error("Failed to unmarshal YAML", "error", err)
		return false, ""
	}

	maintenance := &unstructured.Unstructured{Object: obj}

	// Owner reference enables automatic CR cleanup when Node is deleted
	ownerRef := metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "Node",
		Name:               node.Name,
		UID:                node.UID,
		Controller:         boolPtr(false),
		BlockOwnerDeletion: boolPtr(false),
	}
	maintenance.SetOwnerReferences([]metav1.OwnerReference{ownerRef})

	slog.Info("Added owner reference to CR for automatic garbage collection",
		"node", healthEvent.NodeName,
		"nodeUID", node.UID,
		"crName", crName)

	// Get GVK from the unstructured object
	gvk := maintenance.GroupVersionKind()

	// Convert GVK to GVR using RESTMapper
	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		slog.Error("Failed to get REST mapping", "error", err, "gvk", gvk)
		return false, ""
	}

	// Create the maintenance resource at cluster level
	createdCR, err := c.clientset.Resource(mapping.Resource).
		Create(ctx, maintenance, metav1.CreateOptions{})
	if err != nil {
		return c.handleCreateCRError(ctx, err, crName, healthEvent)
	}

	// Get the actual name of the created CR
	actualCRName := createdCR.GetName()
	log.Printf("Created Maintenance CR %s successfully for node %s", actualCRName, healthEvent.NodeName)

	// Update node annotation with CR reference
	group := common.GetRemediationGroupForAction(healthEvent.RecommendedAction)
	if group != "" && c.annotationManager != nil {
		if err := c.annotationManager.UpdateRemediationState(ctx, healthEvent.NodeName,
			group, actualCRName); err != nil {
			slog.Warn("Failed to update node annotation", "node", healthEvent.NodeName,
				"error", err)
		}
	}

	return true, actualCRName
}

// getNodeForOwnerReference retrieves the node for setting owner reference on the CR.
func (c *FaultRemediationClient) getNodeForOwnerReference(ctx context.Context, nodeName string) (*corev1.Node, error) {
	// Use override function if provided (for testing)
	if c.nodeExistsFunc != nil {
		return c.nodeExistsFunc(ctx, nodeName)
	}

	node, err := c.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			slog.Debug("Node no longer exists, skipping CR creation", "node", nodeName)

			return nil, fmt.Errorf("node not found: %w", err)
		}

		slog.Error("Failed to get node", "node", nodeName, "error", err)

		return nil, fmt.Errorf("failed to get node: %w", err)
	}

	return node, nil
}

func boolPtr(b bool) *bool {
	return &b
}

// handleCreateCRError handles errors from CR creation
func (c *FaultRemediationClient) handleCreateCRError(
	ctx context.Context,
	err error,
	crName string,
	healthEvent *protos.HealthEvent,
) (bool, string) {
	// Check if the CR already exists
	if apierrors.IsAlreadyExists(err) {
		log.Printf("Maintenance CR %s already exists for node %s, treating as success",
			crName, healthEvent.NodeName)

		// Update node annotation with CR reference
		group := common.GetRemediationGroupForAction(healthEvent.RecommendedAction)
		if group != "" && c.annotationManager != nil {
			if err := c.annotationManager.UpdateRemediationState(ctx, healthEvent.NodeName,
				group, crName); err != nil {
				slog.Warn("Failed to update node annotation", "node", healthEvent.NodeName,
					"error", err)
			}
		}

		return true, crName
	}

	// For other errors, log and return failure (not fatal - allow retry)
	log.Printf("Failed to create Maintenance CR: %v", err)

	return false, ""
}

// RunLogCollectorJob creates a log collector Job and waits for completion.
// nolint: cyclop // todo
func (c *FaultRemediationClient) RunLogCollectorJob(ctx context.Context, nodeName string) error {
	if len(c.dryRunMode) > 0 {
		log.Printf("DRY-RUN: Skipping log collector job for node %s", nodeName)
		return nil
	}

	// Read Job manifest
	manifestPath := os.Getenv(LogCollectorManifestPathEnv)
	if manifestPath == "" {
		manifestPath = filepath.Join(c.templateData.TemplateMountPath, "log-collector-job.yaml")
	}

	content, err := os.ReadFile(manifestPath)
	if err != nil {
		logCollectorErrors.WithLabelValues("manifest_read_error", nodeName).Inc()
		return fmt.Errorf("failed to read log collector manifest: %w", err)
	}

	// Create Job from manifest using strong types
	job := &batchv1.Job{}
	if err := yaml.Unmarshal(content, job); err != nil {
		logCollectorErrors.WithLabelValues("manifest_unmarshal_error", nodeName).Inc()
		return fmt.Errorf("failed to unmarshal Job manifest: %w", err)
	}

	// Set target node
	job.Spec.Template.Spec.NodeName = nodeName

	// Create Job using typed client
	created, err := c.kubeClient.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		logCollectorErrors.WithLabelValues("job_creation_error", nodeName).Inc()
		return fmt.Errorf("failed to create Job: %w", err)
	}

	// Wait for completion using typed client with proper watching
	log.Printf("Waiting for log collector job %s to complete", created.Name)

	// Use a context with timeout for the watch
	timeout := 10 * time.Minute // Default timeout: 10 minutes

	if timeoutEnv := os.Getenv("LOG_COLLECTOR_TIMEOUT"); timeoutEnv != "" {
		if parsed, err := time.ParseDuration(timeoutEnv); err == nil {
			timeout = parsed
		} else {
			log.Printf("Warning: Invalid LOG_COLLECTOR_TIMEOUT value '%s', using default 10m: %v", timeoutEnv, err)
		}
	}

	watchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Use SharedInformerFactory for efficient job status monitoring with filtering
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		c.kubeClient,
		30*time.Second, // resync period
		informers.WithNamespace(created.Namespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			// Filter jobs by name to avoid processing irrelevant jobs
			options.FieldSelector = fmt.Sprintf("metadata.name=%s", created.Name)
		}),
	)

	jobInformer := informerFactory.Batch().V1().Jobs()

	// Channel to signal job completion
	done := make(chan error, 1)

	// Add event handler (no need to filter by name since informer is already filtered)
	_, err = jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			job, ok := newObj.(*batchv1.Job)
			if !ok {
				return
			}

			// If job is complete (regardless of pod exit codes), count as success
			// Only count as failure if Kubernetes reports the job itself failed
			// Convert JobConditions to Conditions for meta helper
			conditions := make([]metav1.Condition, len(job.Status.Conditions))
			for i, jc := range job.Status.Conditions {
				conditions[i] = metav1.Condition{
					Type:   string(jc.Type),
					Status: metav1.ConditionStatus(jc.Status),
					Reason: jc.Reason,
				}
			}

			completeCondition := meta.FindStatusCondition(conditions, string(batchv1.JobComplete))
			if completeCondition != nil && completeCondition.Status == metav1.ConditionTrue {
				log.Printf("Log collector job %s completed successfully", created.Name)
				// Use job's actual duration instead of custom tracking
				duration := job.Status.CompletionTime.Sub(job.Status.StartTime.Time).Seconds()

				logCollectorJobs.WithLabelValues(nodeName, "success").Inc()
				logCollectorJobDuration.WithLabelValues(nodeName, "success").Observe(duration)

				done <- nil

				return
			}

			failedCondition := meta.FindStatusCondition(conditions, string(batchv1.JobFailed))
			if failedCondition != nil && failedCondition.Status == metav1.ConditionTrue {
				log.Printf("Log collector job %s failed", created.Name)
				// Use job's actual duration for failed jobs too
				var duration float64
				if job.Status.CompletionTime != nil {
					duration = job.Status.CompletionTime.Sub(job.Status.StartTime.Time).Seconds()
				} else {
					duration = time.Since(job.Status.StartTime.Time).Seconds()
				}

				logCollectorJobs.WithLabelValues(nodeName, "failure").Inc()
				logCollectorJobDuration.WithLabelValues(nodeName, "failure").Observe(duration)

				done <- fmt.Errorf("log collector job %s failed", created.Name)

				return
			}
		},
	})
	if err != nil {
		logCollectorErrors.WithLabelValues("event_handler_error", nodeName).Inc()
		return fmt.Errorf("failed to add event handler for job %s: %w", created.Name, err)
	}

	// Start the informer
	stopCh := make(chan struct{})

	informerFactory.Start(stopCh)

	// Wait for cache to sync
	if !cache.WaitForCacheSync(watchCtx.Done(), jobInformer.Informer().HasSynced) {
		close(stopCh) // Stop informer on sync failure
		logCollectorErrors.WithLabelValues("cache_sync_error", nodeName).Inc()

		return fmt.Errorf("failed to sync cache for job informer")
	}

	// Wait for completion or timeout
	select {
	case <-watchCtx.Done():
		close(stopCh)
		logCollectorJobs.WithLabelValues(nodeName, "timeout").Inc()
		logCollectorErrors.WithLabelValues("job_timeout", nodeName).Inc()

		return fmt.Errorf("timeout waiting for log collector job %s to complete", created.Name)
	case result := <-done:
		close(stopCh)
		return result
	}
}
