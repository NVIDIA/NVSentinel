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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"text/template"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/crstatus"
)

const (
	// Environment variable names
	LogCollectorManifestPathEnv = "LOG_COLLECTOR_MANIFEST_PATH"
)

type FaultRemediationClient struct {
	clientset  dynamic.Interface
	kubeClient kubernetes.Interface
	restMapper *restmapper.DeferredDiscoveryRESTMapper
	dryRunMode []string

	// Multi-template support
	remediationConfig config.TomlConfig
	templates         map[string]*template.Template // map from template file name to parsed template
	templateMountPath string

	annotationManager NodeAnnotationManagerInterface
	statusChecker     *crstatus.CRStatusChecker
	// nodeExistsFunc allows tests to override node existence checking.
	// If nil, uses the default implementation that checks with kubeClient.
	nodeExistsFunc func(ctx context.Context, nodeName string) (*corev1.Node, error)
}

// TemplateData holds the data to be inserted into the template
type TemplateData struct {
	// Node and event data
	NodeName              string
	HealthEventID         string
	RecommendedAction     protos.RecommendedAction
	RecommendedActionName string

	// CRD routing metadata (populated from MaintenanceResource)
	ApiGroup  string
	Version   string
	Kind      string
	Namespace string
}

// NewK8sClient creates a new FaultRemediationClient with multi-template support
// nolint: cyclop // todo
func NewK8sClient(kubeconfig string, dryRun bool, remediationConfig config.TomlConfig) (*FaultRemediationClient,
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

	config.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return auditlogger.NewAuditingRoundTripper(rt)
	})

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

	// Determine template mount path
	templateMountPath := remediationConfig.Template.MountPath
	if templateMountPath == "" {
		return nil, nil, fmt.Errorf("template mount path is not configured")
	}

	// Pre-load and parse all templates
	templates := make(map[string]*template.Template)

	// Load templates for multi-template actions
	for actionName, maintenanceResource := range remediationConfig.RemediationActions {
		if maintenanceResource.TemplateFileName == "" {
			return nil, nil, fmt.Errorf("remediation action %s is missing template file configuration", actionName)
		}

		tmpl, err := loadAndParseTemplate(templateMountPath, maintenanceResource.TemplateFileName, actionName)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load template for action %s: %w", actionName, err)
		}

		templates[actionName] = tmpl
	}

	// Validate namespace configuration for namespaced resources
	for actionName, maintenanceResource := range remediationConfig.RemediationActions {
		if maintenanceResource.Scope == "Namespaced" && maintenanceResource.Namespace == "" {
			return nil, nil, fmt.Errorf("remediation action %s is namespaced but missing namespace configuration", actionName)
		}
	}

	client := &FaultRemediationClient{
		clientset:         clientset,
		kubeClient:        kubeClient,
		restMapper:        mapper,
		remediationConfig: remediationConfig,
		templates:         templates,
		templateMountPath: templateMountPath,
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
		remediationConfig.RemediationActions,
		dryRun,
	)

	return client, kubeClient, nil
}

// loadAndParseTemplate loads and parses a template file
func loadAndParseTemplate(mountPath, fileName, templateName string) (*template.Template, error) {
	templatePath := filepath.Join(mountPath, fileName)

	// Check if the template file exists
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("template file does not exist: %s", templatePath)
	}

	// Read and parse the template
	templateContent, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("error reading template file: %w", err)
	}

	tmpl := template.New(templateName)

	tmpl, err = tmpl.Parse(string(templateContent))
	if err != nil {
		return nil, fmt.Errorf("error parsing template: %w", err)
	}

	return tmpl, nil
}

func (c *FaultRemediationClient) GetAnnotationManager() NodeAnnotationManagerInterface {
	return c.annotationManager
}

func (c *FaultRemediationClient) GetStatusChecker() *crstatus.CRStatusChecker {
	return c.statusChecker
}

// GetConfig returns the remediation configuration for this client.
// This includes the multi-template actions and their associated maintenance resources.
func (c *FaultRemediationClient) GetConfig() *config.TomlConfig {
	return &c.remediationConfig
}

func (c *FaultRemediationClient) CreateMaintenanceResource(
	ctx context.Context,
	healthEventData *HealthEventData,
) (bool, string) {
	healthEvent := healthEventData.HealthEvent
	healthEventID := healthEventData.ID

	recommendedActionName := healthEvent.RecommendedAction.String()

	maintenanceResource, selectedTemplate, actionKey, ok :=
		c.selectRemediationActionAndTemplate(recommendedActionName, healthEvent.NodeName)
	if !ok {
		return false, ""
	}

	crName := fmt.Sprintf("maintenance-%s-%s", healthEvent.NodeName, healthEventID)

	if len(c.dryRunMode) > 0 {
		slog.Info("DRY-RUN: Skipping custom resource creation",
			"node", healthEvent.NodeName,
			"template", actionKey)

		return true, crName
	}

	node, err := c.getNodeForOwnerReference(ctx, healthEvent.NodeName)
	if err != nil {
		slog.Warn("Failed to get node for owner reference, skipping CR creation",
			"node", healthEvent.NodeName,
			"error", err)

		return false, ""
	}

	slog.Info("Creating maintenance CR",
		"node", healthEvent.NodeName,
		"template", actionKey,
		"nodeUID", node.UID)

	templateData := TemplateData{
		NodeName:              healthEvent.NodeName,
		HealthEventID:         healthEventID,
		RecommendedAction:     healthEvent.RecommendedAction,
		RecommendedActionName: recommendedActionName,

		ApiGroup:  maintenanceResource.ApiGroup,
		Version:   maintenanceResource.Version,
		Kind:      maintenanceResource.Kind,
		Namespace: maintenanceResource.Namespace,
	}

	maintenance, yamlStr, err := renderMaintenanceFromTemplate(selectedTemplate, templateData)
	if err != nil {
		slog.Error("Failed to render maintenance template",
			"template", actionKey,
			"error", err)

		return false, ""
	}

	slog.Debug("Generated YAML from template",
		"template", actionKey,
		"yaml", yamlStr)

	setNodeOwnerRef(maintenance, node)

	gvk := maintenance.GroupVersionKind()

	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		slog.Error("Failed to get REST mapping", "error", err, "gvk", gvk)

		return false, ""
	}

	createdCR, err := c.createMaintenanceCR(ctx, mapping.Resource, maintenanceResource, maintenance)
	if err != nil {
		return c.handleCreateCRError(ctx, err, crName, healthEvent)
	}

	actualCRName := createdCR.GetName()
	slog.Info("Created Maintenance CR successfully",
		"crName", actualCRName,
		"node", healthEvent.NodeName,
		"template", actionKey)

	c.maybeUpdateRemediationAnnotation(ctx, healthEvent.NodeName, maintenanceResource, actualCRName, recommendedActionName)

	return true, actualCRName
}

func (c *FaultRemediationClient) selectRemediationActionAndTemplate(
	recommendedActionName string,
	nodeName string,
) (config.MaintenanceResource, *template.Template, string, bool) {
	resource, exists := c.remediationConfig.RemediationActions[recommendedActionName]
	if !exists {
		slog.Error("No remediation configuration found for action",
			"action", recommendedActionName,
			"node", nodeName,
			"availableActions", func() []string {
				actions := make([]string, 0, len(c.remediationConfig.RemediationActions))
				for action := range c.remediationConfig.RemediationActions {
					actions = append(actions, action)
				}

				return actions
			}())

		return config.MaintenanceResource{}, nil, "", false
	}

	tmpl := c.templates[recommendedActionName]
	if tmpl == nil {
		slog.Error("No template available for remediation action",
			"action", recommendedActionName,
			"node", nodeName)

		return config.MaintenanceResource{}, nil, "", false
	}

	return resource, tmpl, recommendedActionName, true
}

func renderMaintenanceFromTemplate(
	tmpl *template.Template,
	data TemplateData,
) (*unstructured.Unstructured, string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, "", err
	}

	var obj map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &obj); err != nil {
		return nil, "", err
	}

	return &unstructured.Unstructured{Object: obj}, buf.String(), nil
}

func setNodeOwnerRef(maintenance *unstructured.Unstructured, node *corev1.Node) {
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
		"node", node.Name,
		"nodeUID", node.UID,
		"crName", maintenance.GetName())
}

func (c *FaultRemediationClient) createMaintenanceCR(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	mr config.MaintenanceResource,
	maintenance *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	if mr.Scope == "Namespaced" {
		return c.clientset.Resource(gvr).
			Namespace(mr.Namespace).
			Create(ctx, maintenance, metav1.CreateOptions{})
	}

	return c.clientset.Resource(gvr).
		Create(ctx, maintenance, metav1.CreateOptions{})
}

func (c *FaultRemediationClient) maybeUpdateRemediationAnnotation(
	ctx context.Context,
	nodeName string,
	mr config.MaintenanceResource,
	actualCRName string,
	recommendedActionName string,
) {
	group := mr.EquivalenceGroup
	if group == "" || c.annotationManager == nil {
		return
	}

	if err := c.annotationManager.UpdateRemediationState(
		ctx, nodeName, group, actualCRName, recommendedActionName,
	); err != nil {
		slog.Warn("Failed to update node annotation",
			"node", nodeName,
			"error", err)
	}
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
		slog.Info("Maintenance CR already exists, treating as success",
			"crName", crName,
			"node", healthEvent.NodeName)

		// Update node annotation with CR reference
		actionName := healthEvent.RecommendedAction.String()
		actionConfig, exists := c.remediationConfig.RemediationActions[actionName]

		if !exists {
			return true, crName
		}

		group := actionConfig.EquivalenceGroup
		if group == "" {
			return true, crName
		}

		if c.annotationManager == nil {
			return true, crName
		}

		if err := c.annotationManager.UpdateRemediationState(ctx, healthEvent.NodeName,
			group, crName, actionName); err != nil {
			slog.Warn("Failed to update node annotation", "node", healthEvent.NodeName,
				"error", err)
		}

		return true, crName
	}

	// For other errors, log and return failure (not fatal - allow retry)
	slog.Error("Failed to create Maintenance CR",
		"error", err)

	return false, ""
}

// RunLogCollectorJob creates a log collector Job and waits for completion.
// nolint: cyclop // todo
func (c *FaultRemediationClient) RunLogCollectorJob(ctx context.Context, nodeName string) error {
	if len(c.dryRunMode) > 0 {
		slog.Info("DRY-RUN: Skipping log collector job", "node", nodeName)
		return nil
	}

	// Read Job manifest
	manifestPath := os.Getenv(LogCollectorManifestPathEnv)
	if manifestPath == "" {
		manifestPath = filepath.Join(c.templateMountPath, "log-collector-job.yaml")
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
	slog.Info("Waiting for log collector job to complete", "job", created.Name)

	// Use a context with timeout for the watch
	timeout := 10 * time.Minute // Default timeout: 10 minutes

	if timeoutEnv := os.Getenv("LOG_COLLECTOR_TIMEOUT"); timeoutEnv != "" {
		if parsed, err := time.ParseDuration(timeoutEnv); err == nil {
			timeout = parsed
		} else {
			slog.Warn("Invalid LOG_COLLECTOR_TIMEOUT value, using default 10m", "value", timeoutEnv, "error", err)
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
				slog.Info("Log collector job completed successfully", "job", created.Name)
				// Use job's actual duration instead of custom tracking
				duration := job.Status.CompletionTime.Sub(job.Status.StartTime.Time).Seconds()

				logCollectorJobs.WithLabelValues(nodeName, "success").Inc()
				logCollectorJobDuration.WithLabelValues(nodeName, "success").Observe(duration)

				done <- nil

				return
			}

			failedCondition := meta.FindStatusCondition(conditions, string(batchv1.JobFailed))
			if failedCondition != nil && failedCondition.Status == metav1.ConditionTrue {
				slog.Error("Log collector job failed", "job", created.Name)
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
	synced := cache.WaitForCacheSync(watchCtx.Done(), jobInformer.Informer().HasSynced)
	if !synced {
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
