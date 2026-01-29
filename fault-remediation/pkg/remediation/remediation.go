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

package remediation

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/nvidia/nvsentinel/fault-remediation/pkg/annotation"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/crstatus"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/events"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/metrics"
)

const (
	logCollectorNodeLabel              = "dgxc.nvidia.com/node-name"
	logCollectorEventLabel             = "dgxc.nvidia.com/event-id"
	jobMetricsAlreadyCountedAnnotation = "dgxc.nvidia.com/logcollector-metrics-recorded"
	trueStringVal                      = "true"
	controllerRequeueDuration          = 10 * time.Second

	// Environment variable names
	LogCollectorManifestPathEnv = "LOG_COLLECTOR_MANIFEST_PATH"
)

type FaultRemediationClient struct {
	client     client.Client
	dryRunMode []string

	// Multi-template support
	remediationConfig config.TomlConfig
	templates         map[string]*template.Template // map from template file name to parsed template
	templateMountPath string

	annotationManager annotation.NodeAnnotationManagerInterface
	statusChecker     *crstatus.CRStatusChecker
}

func NewRemediationClient(
	client client.Client,
	dryRun bool,
	remediationConfig config.TomlConfig,
) (*FaultRemediationClient, error) {
	// Determine template mount path
	templateMountPath := remediationConfig.Template.MountPath
	if templateMountPath == "" {
		return nil, fmt.Errorf("template mount path is not configured")
	}

	// Pre-load and parse all templates
	templates := make(map[string]*template.Template)

	// Load templates for multi-template actions
	for actionName, maintenanceResource := range remediationConfig.RemediationActions {
		if maintenanceResource.TemplateFileName == "" {
			return nil, fmt.Errorf("remediation action %s is missing template file configuration", actionName)
		}

		tmpl, err := loadAndParseTemplate(templateMountPath, maintenanceResource.TemplateFileName, actionName)
		if err != nil {
			return nil, fmt.Errorf("failed to load template for action %s: %w", actionName, err)
		}

		templates[actionName] = tmpl
	}

	// Validate namespace configuration for namespaced resources
	for actionName, maintenanceResource := range remediationConfig.RemediationActions {
		if maintenanceResource.Scope == "Namespaced" && maintenanceResource.Namespace == "" {
			return nil, fmt.Errorf("remediation action %s is namespaced but missing namespace configuration", actionName)
		}
	}

	ctrlRuntimeRemediationClient := &FaultRemediationClient{
		client:            client,
		templates:         templates,
		templateMountPath: templateMountPath,
		remediationConfig: remediationConfig,
	}

	if dryRun {
		ctrlRuntimeRemediationClient.dryRunMode = []string{metav1.DryRunAll}
	} else {
		ctrlRuntimeRemediationClient.dryRunMode = []string{}
	}

	// Initialize annotation manager
	ctrlRuntimeRemediationClient.annotationManager = annotation.NewNodeAnnotationManager(client)

	ctrlRuntimeRemediationClient.statusChecker = crstatus.NewCRStatusChecker(
		client,
		remediationConfig.RemediationActions,
		dryRun,
	)

	return ctrlRuntimeRemediationClient, nil
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

func (c *FaultRemediationClient) GetAnnotationManager() annotation.NodeAnnotationManagerInterface {
	return c.annotationManager
}

func (c *FaultRemediationClient) GetStatusChecker() crstatus.CRStatusCheckerInterface {
	return c.statusChecker
}

// nolint: cyclop // todo
func (c *FaultRemediationClient) CreateMaintenanceResource(ctx context.Context, healthEventData *events.HealthEventData,
	groupConfig *common.EquivalenceGroupConfig) (string, error) {
	healthEvent := healthEventData.HealthEvent
	healthEventID := healthEventData.ID

	// Generate CR name
	crName := fmt.Sprintf("maintenance-%s-%s", healthEvent.NodeName, healthEventID)

	// Skip custom resource creation if dry-run is enabled
	if len(c.dryRunMode) > 0 {
		slog.Info("DRY-RUN: Skipping custom resource creation", "node", healthEvent.NodeName)
		return crName, nil
	}

	recommendedActionName := healthEvent.RecommendedAction.String()

	maintenanceResource, selectedTemplate, actionKey, err :=
		c.selectRemediationActionAndTemplate(recommendedActionName, healthEvent.NodeName)
	if err != nil {
		return "", fmt.Errorf("error selecting remediation action and template: %w", err)
	}

	// Get the node object to extract UID for owner reference
	// This also verifies the node exists before creating CR
	node, err := c.getNodeForOwnerReference(ctx, healthEvent.NodeName)
	if err != nil {
		slog.Warn("Failed to get node for owner reference, skipping CR creation",
			"node", healthEvent.NodeName,
			"error", err)

		return "", fmt.Errorf("failed to get node for owner reference: %w", err)
	}

	slog.Info("Creating maintenance CR",
		"node", healthEvent.NodeName,
		"template", actionKey,
		"nodeUID", node.UID)

	templateData := TemplateData{
		NodeName:                 healthEvent.NodeName,
		HealthEventID:            healthEventID,
		RecommendedAction:        healthEvent.RecommendedAction,
		RecommendedActionName:    recommendedActionName,
		ImpactedEntityScopeValue: groupConfig.ImpactedEntityScopeValue,

		HealthEvent: healthEvent,

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

		return "", fmt.Errorf("error rendering maintenance template: %w", err)
	}

	slog.Debug("Generated YAML from template",
		"template", actionKey,
		"yaml", yamlStr)

	setNodeOwnerRef(maintenance, node)

	err = c.client.Create(ctx, maintenance)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			slog.Info("Maintenance CR already exists for node, treating as success", "CR",
				crName, "node", healthEvent.NodeName)
		} else {
			return "", fmt.Errorf("failed to create maintenance CR: %w", err)
		}
	}

	actualCRName := maintenance.GetName()
	slog.Info("Created Maintenance CR successfully",
		"crName", actualCRName,
		"node", healthEvent.NodeName,
		"template", actionKey)

	if len(groupConfig.EffectiveEquivalenceGroup) != 0 && c.annotationManager != nil {
		if err = c.annotationManager.UpdateRemediationState(ctx, healthEvent.NodeName,
			groupConfig.EffectiveEquivalenceGroup, actualCRName, recommendedActionName); err != nil {
			slog.Warn("Failed to update node annotation", "node", healthEvent.NodeName,
				"error", err)

			return "", fmt.Errorf("failed to update node annotation: %w", err)
		}
	}

	return actualCRName, nil
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
		Controller:         ptr.To(false),
		BlockOwnerDeletion: ptr.To(false),
	}
	maintenance.SetOwnerReferences([]metav1.OwnerReference{ownerRef})

	slog.Info("Added owner reference to CR for automatic garbage collection",
		"node", node.Name,
		"nodeUID", node.UID,
		"crName", maintenance.GetName())
}

func (c *FaultRemediationClient) selectRemediationActionAndTemplate(
	recommendedActionName string,
	nodeName string,
) (config.MaintenanceResource, *template.Template, string, error) {
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

		return config.MaintenanceResource{}, nil, "", fmt.Errorf("no remediation config for action")
	}

	tmpl := c.templates[recommendedActionName]
	if tmpl == nil {
		slog.Error("No template available for remediation action",
			"action", recommendedActionName,
			"node", nodeName)

		return config.MaintenanceResource{}, nil, "", fmt.Errorf("no template available for remediation action")
	}

	return resource, tmpl, recommendedActionName, nil
}

// getNodeForOwnerReference retrieves the node for setting owner reference on the CR.
func (c *FaultRemediationClient) getNodeForOwnerReference(
	ctx context.Context,
	nodeName string,
) (*corev1.Node, error) {
	node := &corev1.Node{}

	err := c.client.Get(ctx, types.NamespacedName{
		Name: nodeName,
	}, node)
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

// RunLogCollectorJob creates a log collector Job and waits for completion.
func (c *FaultRemediationClient) RunLogCollectorJob(
	ctx context.Context,
	nodeName string,
	eventUID string,
) (ctrl.Result, error) {
	if len(c.dryRunMode) > 0 {
		slog.Info("DRY-RUN: Skipping log collector job for node", "node", nodeName)
		return ctrl.Result{}, nil
	}

	job, result, err := c.launchLogCollectorJob(ctx, nodeName, eventUID)
	if err != nil || !result.IsZero() {
		return result, err
	}

	return c.checkLogCollectorStatus(ctx, nodeName, job)
}

func (c *FaultRemediationClient) launchLogCollectorJob(
	ctx context.Context,
	nodeName string,
	eventUID string,
) (batchv1.Job, ctrl.Result, error) {
	// Read Job manifest
	manifestPath := os.Getenv(LogCollectorManifestPathEnv)
	if manifestPath == "" {
		manifestPath = filepath.Join(c.templateMountPath, "log-collector-job.yaml")
	}

	content, err := os.ReadFile(manifestPath)
	if err != nil {
		metrics.LogCollectorErrors.WithLabelValues("manifest_read_error", nodeName).Inc()
		return batchv1.Job{}, ctrl.Result{}, fmt.Errorf("failed to read log collector manifest: %w", err)
	}

	// Create Job from manifest using strong types
	job := &batchv1.Job{}
	if err = yaml.Unmarshal(content, job); err != nil {
		metrics.LogCollectorErrors.WithLabelValues("manifest_unmarshal_error", nodeName).Inc()
		return batchv1.Job{}, ctrl.Result{}, fmt.Errorf("failed to unmarshal Job manifest: %w", err)
	}

	// Set target node
	job.Spec.Template.Spec.NodeName = nodeName
	if job.Labels == nil {
		job.Labels = map[string]string{}
	}

	labels := map[string]string{
		logCollectorNodeLabel:  nodeName,
		logCollectorEventLabel: eventUID,
	}
	for k, v := range labels {
		job.Labels[k] = v
	}

	// Get job if exists otherwise create
	existingJobs := &batchv1.JobList{}

	err = c.client.List(
		ctx,
		existingJobs,
		client.MatchingLabels(labels),
		client.InNamespace(job.GetNamespace()),
	)
	if err != nil {
		return batchv1.Job{}, ctrl.Result{}, err
	}

	// There should not be multiple jobs for same event, in this case return error
	// this will then requeue and wait until the jobs clear
	if len(existingJobs.Items) > 1 {
		return batchv1.Job{},
			ctrl.Result{},
			fmt.Errorf("expecting zero or one log collector job per event per node, found %v", len(existingJobs.Items))
	}

	if len(existingJobs.Items) == 0 {
		err = c.client.Create(ctx, job)
		if err != nil {
			return batchv1.Job{}, ctrl.Result{}, err
		}
		// if created, requeue to check status later
		return batchv1.Job{}, ctrl.Result{RequeueAfter: controllerRequeueDuration}, nil
	}

	return existingJobs.Items[0], ctrl.Result{}, nil
}

func (c *FaultRemediationClient) checkLogCollectorStatus(
	ctx context.Context,
	nodeName string,
	job batchv1.Job,
) (ctrl.Result, error) {
	// convert to metav1 condition
	conditions := make([]metav1.Condition, len(job.Status.Conditions))
	for i, jc := range job.Status.Conditions {
		conditions[i] = metav1.Condition{
			Type:   string(jc.Type),
			Status: metav1.ConditionStatus(jc.Status),
			Reason: jc.Reason,
		}
	}

	found, err := c.checkLogCollectorComplete(ctx, nodeName, job, conditions)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check log collector status: %w", err)
	}

	if found {
		return ctrl.Result{}, nil
	}

	found, err = c.checkLogCollectorFailed(ctx, nodeName, job, conditions)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check log collector status: %w", err)
	}

	if found {
		return ctrl.Result{}, nil
	}

	found, err = c.checkLogCollectorTimedOut(ctx, nodeName, job)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check log collector status: %w", err)
	}

	if found {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: controllerRequeueDuration}, nil
}

func (c *FaultRemediationClient) checkLogCollectorComplete(
	ctx context.Context,
	nodeName string,
	job batchv1.Job,
	conditions []metav1.Condition,
) (bool, error) {
	completeCondition := meta.FindStatusCondition(conditions, string(batchv1.JobComplete))
	//nolint:nestif // todo
	if completeCondition != nil && completeCondition.Status == metav1.ConditionTrue {
		slog.Info("Log collector job completed successfully", "job", job.Name)
		// Use job's actual duration instead of custom tracking
		// reconciliation can be called multiple times so use annotation to make sure we're not duplicate recording metrics
		if job.Annotations == nil || job.Annotations[jobMetricsAlreadyCountedAnnotation] != trueStringVal {
			updateJob := job.DeepCopy()
			if updateJob.Annotations == nil {
				updateJob.Annotations = map[string]string{}
			}

			updateJob.Annotations[jobMetricsAlreadyCountedAnnotation] = trueStringVal

			err := c.client.Update(ctx, updateJob)
			if err != nil {
				return false, err
			}

			metrics.LogCollectorJobs.WithLabelValues(nodeName, "success").Inc()

			if job.Status.StartTime != nil {
				duration := job.Status.CompletionTime.Sub(job.Status.StartTime.Time).Seconds()
				metrics.LogCollectorJobDuration.WithLabelValues(nodeName, "success").Observe(duration)
			}
		}

		return true, nil
	}

	return false, nil
}

func (c *FaultRemediationClient) checkLogCollectorFailed(
	ctx context.Context,
	nodeName string,
	job batchv1.Job,
	conditions []metav1.Condition,
) (bool, error) {
	// check if failed
	failedCondition := meta.FindStatusCondition(conditions, string(batchv1.JobFailed))
	//nolint:nestif // todo
	if failedCondition != nil && failedCondition.Status == metav1.ConditionTrue {
		slog.Info("Log collector job failed", "job", job.Name)

		// reconciliation can be called multiple times so use annotation to make sure we're not duplicate recording metrics
		if job.Annotations == nil || job.Annotations[jobMetricsAlreadyCountedAnnotation] != trueStringVal {
			updateJob := job.DeepCopy()
			if updateJob.Annotations == nil {
				updateJob.Annotations = map[string]string{}
			}

			updateJob.Annotations[jobMetricsAlreadyCountedAnnotation] = trueStringVal

			err := c.client.Update(ctx, updateJob)
			if err != nil {
				return false, err
			}

			// Use job's actual duration for failed jobs too
			var duration float64
			if job.Status.CompletionTime != nil {
				duration = job.Status.CompletionTime.Sub(job.Status.StartTime.Time).Seconds()
			} else {
				duration = time.Since(job.Status.StartTime.Time).Seconds()
			}

			metrics.LogCollectorJobs.WithLabelValues(nodeName, "failure").Inc()
			metrics.LogCollectorJobDuration.WithLabelValues(nodeName, "failure").Observe(duration)
		}

		// dont return error so reconciliation can continue
		return true, nil
	}

	return false, nil
}

func (c *FaultRemediationClient) checkLogCollectorTimedOut(
	ctx context.Context,
	nodeName string,
	job batchv1.Job,
) (bool, error) {
	timeout := 10 * time.Minute // Default timeout: 10 minutes

	if timeoutEnv := os.Getenv("LOG_COLLECTOR_TIMEOUT"); timeoutEnv != "" {
		if parsed, err := time.ParseDuration(timeoutEnv); err == nil {
			timeout = parsed
		} else {
			slog.Warn("Invalid LOG_COLLECTOR_TIMEOUT value, using default 10m", "timeout-value", timeoutEnv, "error", err)
		}
	}

	// check timeout
	//nolint:nestif // todo
	if time.Since(job.CreationTimestamp.Time) > timeout {
		slog.Info("Log collector job past timeout", "job", job.Name, "timeout", timeout)

		if job.Annotations == nil || job.Annotations[jobMetricsAlreadyCountedAnnotation] != trueStringVal {
			updateJob := job.DeepCopy()
			if updateJob.Annotations == nil {
				updateJob.Annotations = map[string]string{}
			}

			updateJob.Annotations[jobMetricsAlreadyCountedAnnotation] = trueStringVal

			err := c.client.Update(ctx, updateJob)
			if err != nil {
				return false, err
			}

			metrics.LogCollectorJobs.WithLabelValues(nodeName, "timeout").Inc()
			metrics.LogCollectorJobDuration.WithLabelValues(nodeName, "timeout").Observe(timeout.Seconds())
		}

		return true, nil
	}

	return false, nil
}

func (c *FaultRemediationClient) GetConfig() *config.TomlConfig {
	return &c.remediationConfig
}
