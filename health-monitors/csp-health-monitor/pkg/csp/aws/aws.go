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

package aws

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/health"
	"github.com/aws/aws-sdk-go-v2/service/health/types"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	eventpkg "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	klog "k8s.io/klog/v2"
)

const (
	INSTANCE_NETWORK_MAINTENANCE_SCHEDULED       = "AWS_EC2_INSTANCE_NETWORK_MAINTENANCE_SCHEDULED"
	INSTANCE_POWER_MAINTENANCE_SCHEDULED         = "AWS_EC2_INSTANCE_POWER_MAINTENANCE_SCHEDULED"
	INSTANCE_REBOOT_MAINTENANCE_SCHEDULED        = "AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED"
	MAINTENANCE_SCHEDULED                        = "AWS_EC2_MAINTENANCE_SCHEDULED"
	DEDICATED_HOST_MAINTENANCE_SCHEDULED         = "AWS_EC2_DEDICATED_HOST_MAINTENANCE_SCHEDULED"
	DEDICATED_HOST_NETWORK_MAINTENANCE_SCHEDULED = "AWS_EC2_DEDICATED_HOST_NETWORK_MAINTENANCE_SCHEDULED"
	DEDICATED_HOST_POWER_MAINTENANCE_SCHEDULED   = "AWS_EC2_DEDICATED_HOST_POWER_MAINTENANCE_SCHEDULED"
)

var SupportedEventTypeCodes = map[string]string{
	INSTANCE_NETWORK_MAINTENANCE_SCHEDULED:       "",
	INSTANCE_POWER_MAINTENANCE_SCHEDULED:         "",
	INSTANCE_REBOOT_MAINTENANCE_SCHEDULED:        "",
	MAINTENANCE_SCHEDULED:                        "",
	DEDICATED_HOST_MAINTENANCE_SCHEDULED:         "",
	DEDICATED_HOST_NETWORK_MAINTENANCE_SCHEDULED: "",
	DEDICATED_HOST_POWER_MAINTENANCE_SCHEDULED:   "",
}

var SupportedEventTypeCodesList = []string{
	INSTANCE_NETWORK_MAINTENANCE_SCHEDULED,
	INSTANCE_POWER_MAINTENANCE_SCHEDULED,
	INSTANCE_REBOOT_MAINTENANCE_SCHEDULED,
	MAINTENANCE_SCHEDULED,
	DEDICATED_HOST_MAINTENANCE_SCHEDULED,
	DEDICATED_HOST_NETWORK_MAINTENANCE_SCHEDULED,
	DEDICATED_HOST_POWER_MAINTENANCE_SCHEDULED,
}

// isSupportedEventTypeCode reports whether code is one of the supported maintenance event codes.
func isSupportedEventTypeCode(code string) bool {
	_, ok := SupportedEventTypeCodes[code]
	return ok
}

// healthClientInterface defines the AWS Health API methods we use
type healthClientInterface interface {
	DescribeEvents(
		ctx context.Context,
		params *health.DescribeEventsInput,
		optFns ...func(*health.Options),
	) (*health.DescribeEventsOutput, error)
	DescribeAffectedEntities(
		ctx context.Context,
		params *health.DescribeAffectedEntitiesInput,
		optFns ...func(*health.Options),
	) (*health.DescribeAffectedEntitiesOutput, error)
	DescribeEventDetails(
		ctx context.Context,
		params *health.DescribeEventDetailsInput,
		optFns ...func(*health.Options),
	) (*health.DescribeEventDetailsOutput, error)
}

// AWSClient implements the csp.Monitor interface for AWS.
type AWSClient struct {
	config         config.AWSConfig
	awsClient      healthClientInterface
	k8sClient      kubernetes.Interface
	normalizer     eventpkg.Normalizer
	clusterName    string
	kubeconfigPath string
}

// NewClient creates a new AWS client.
func NewClient(
	ctx context.Context,
	cfg config.AWSConfig,
	clusterName string,
	kubeconfigPath string,
	store datastore.Store,
) (*AWSClient, error) {
	// default: max attempts = 3, back off delay = 20s
	awsSDKConfig, err := awsConfig.LoadDefaultConfig(ctx,
		awsConfig.WithRegion(cfg.Region),
	)
	if err != nil {
		metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "aws_sdk_config_error").Inc()
		return nil, fmt.Errorf("failed to load AWS SDK config for region %s: %w",
			cfg.Region, err)
	}

	healthAPIClient := health.NewFromConfig(awsSDKConfig)

	klog.Infof("Successfully initialized AWS Health client for region %s", cfg.Region)

	var k8sClient kubernetes.Interface

	var k8sRestConfig *rest.Config

	if kubeconfigPath != "" {
		klog.Infof("AWS Client: Using kubeconfig from path: %s", kubeconfigPath)
		k8sRestConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		klog.Info("AWS Client: KubeconfigPath not specified, attempting in-cluster config.")

		k8sRestConfig, err = rest.InClusterConfig()
	}

	if err != nil {
		metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "k8s_config_error").Inc()

		return nil, fmt.Errorf(
			"AWS client failed to initialize K8s config (kubeconfig: '%s'): %w",
			kubeconfigPath, err,
		)
	}

	k8sClient, err = kubernetes.NewForConfig(k8sRestConfig)
	if err != nil {
		metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "k8s_clientset_error").Inc()
		return nil, fmt.Errorf("AWS client failed to create K8s clientset: %w", err)
	}

	klog.Info("AWS Client: Kubernetes clientset initialized successfully.")

	normalizer, err := eventpkg.GetNormalizer(model.CSPAWS)
	if err != nil {
		return nil, fmt.Errorf("failed to get AWS normalizer: %w", err)
	}

	return &AWSClient{
		config:         cfg,
		awsClient:      healthAPIClient,
		k8sClient:      k8sClient,
		normalizer:     normalizer,
		clusterName:    clusterName,
		kubeconfigPath: kubeconfigPath,
	}, nil
}

// GetName returns "aws".
func (c *AWSClient) GetName() model.CSP {
	return model.CSPAWS
}

// StartMonitoring polls the AWS Health API periodically.
func (c *AWSClient) StartMonitoring(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error {
	klog.Infof(
		"Starting AWS Health API polling every %d seconds in region %s",
		c.config.PollingIntervalSeconds,
		c.config.Region,
	)

	ticker := time.NewTicker(time.Duration(c.config.PollingIntervalSeconds) * time.Second)
	defer ticker.Stop()

	if err := c.pollEvents(ctx, eventChan); err != nil {
		klog.Errorf("Initial error polling AWS Health events: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			klog.Infof("Context cancelled, AWS monitoring stopped.\n")
			return ctx.Err()
		case <-ticker.C:
			err := c.pollEvents(ctx, eventChan)
			if err != nil {
				metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "poll_events_error").Inc()
				klog.Errorf("Error polling AWS Health events: %v\n", err)
			}
		}
	}
}

// pollEvents performs a single poll request to the AWS Health API.
func (c *AWSClient) pollEvents(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error {
	pollStart := time.Now()
	defer func() {
		metrics.CSPPollingDuration.WithLabelValues(string(model.CSPAWS)).Observe(time.Since(pollStart).Seconds())
	}()

	klog.V(2).Infof("Polling AWS Health API...")

	instanceIDs, err := c.getClusterInstanceNodeMap(ctx)
	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPAWS), "get_nodes_provider_id_error").Inc()
		klog.Errorf("Error getting nodes provider IDs: %v\n", err)

		return fmt.Errorf("error getting nodes provider IDs: %w", err)
	}

	klog.V(2).Infof("Found nodes with instance IDs: %v", instanceIDs)

	err = c.handleMaintenanceEvents(ctx, instanceIDs, eventChan)
	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPAWS), "handle_maintenance_events_error").Inc()
		klog.Errorf("Error polling AWS Health events: %v\n", err)

		return fmt.Errorf("error polling AWS Health events: %w", err)
	}

	return nil
}

// handleMaintenanceEvents performs a single poll request to the AWS Health API.
func (c *AWSClient) handleMaintenanceEvents(
	ctx context.Context,
	instanceIDs map[string]string,
	eventChan chan<- model.MaintenanceEvent,
) error {
	now := time.Now().UTC()
	pollStartTime := time.Now().UTC().Add(-time.Duration(c.config.PollingIntervalSeconds) * time.Second)

	klog.V(2).Infof("AWS Poll Window: StartTime >= %v, EndTime <= %v",
		pollStartTime, now)

	events, err := c.pollEventsAPI(ctx, pollStartTime)
	if err != nil {
		klog.Errorf("Error polling AWS Health events: %v\n", err)
		return err
	}

	if len(events) == 0 {
		klog.V(3).Infof("No AWS EC2 maintenance scheduled for this poll window")
		return nil
	}

	eventArnsMap := make(map[string]types.Event)

	for _, event := range events {
		metrics.CSPEventsReceived.WithLabelValues(string(model.CSPAWS)).Inc()

		if event.EventTypeCode == nil {
			klog.V(4).Infof("Skipping event %s: EventTypeCode is nil", aws.ToString(event.Arn))
			continue
		}

		if !isSupportedEventTypeCode(*event.EventTypeCode) {
			metrics.CSPEventsByTypeUnsupported.WithLabelValues(string(model.CSPAWS), *event.EventTypeCode).Inc()
			klog.V(3).Infof("Ignoring unsupported event type code %s for event %s",
				*event.EventTypeCode, aws.ToString(event.Arn))

			continue
		}

		klog.V(3).Infof(
			"Adding supported maintenance event %s (%s) for further processing",
			aws.ToString(event.Arn), *event.EventTypeCode,
		)

		eventArnsMap[*event.Arn] = event
	}

	klog.Infof("Found %d AWS maintenance scheduled events", len(eventArnsMap))

	var wg sync.WaitGroup

	for eventID, event := range eventArnsMap {
		wg.Add(1)

		go func(eventID string, eventData types.Event) {
			defer wg.Done()
			c.processAWSHealthEvent(ctx, eventID, eventData, instanceIDs, eventChan)
		}(eventID, event)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	return nil
}

// processSingleEntityForEvent processes a single affected entity for a given AWS health event.
func (c *AWSClient) processSingleEntityForEvent(
	ctx context.Context,
	eventArn string,
	evt types.Event,
	entity types.AffectedEntity,
	desc string,
	action pb.RecommenedAction,
	nodeMap map[string]string,
	eventChan chan<- model.MaintenanceEvent,
) {
	if entity.EntityValue == nil {
		klog.Warningf("Entity with nil EntityValue for event %s", eventArn)
		return
	}

	instanceID := *entity.EntityValue

	nodeName, ok := nodeMap[instanceID]
	if !ok {
		// Not an instance in our cluster, or mapping failed. Already logged by
		// getClusterInstanceNodeMap if node has no providerID.
		// klog.V(4).Infof("Instance %s from event %s not found in current cluster node map", instanceID, eventArn)
		return
	}

	if entity.EventArn == nil || entity.EntityArn == nil {
		klog.Warningf(
			"Affected entity for instance %s (event %s) doesn't have complete information. Missing EventArn or EntityArn: %+v",
			instanceID,
			eventArn,
			entity,
		)

		return
	}

	eventMetadata := eventpkg.EventMetadata{
		Event:            evt,
		NodeName:         nodeName,
		InstanceId:       instanceID,
		EntityArn:        aws.ToString(entity.EntityArn),
		Action:           action.String(),
		EventDescription: desc,
	}

	normalizedEvent, err := c.normalizer.Normalize(evt, eventMetadata)
	if err != nil {
		metrics.MainNormalizationErrors.WithLabelValues(string(model.CSPAWS)).Inc()
		klog.Errorf(
			"Error normalizing AWS event for node %s (instance %s, event %s): %v",
			nodeName,
			instanceID,
			eventArn,
			err,
		)

		return
	}

	metrics.MainEventsToNormalize.WithLabelValues(string(model.CSPAWS)).Inc()
	select {
	case eventChan <- *normalizedEvent:
		klog.Infof(
			"Dispatched maintenance event for node %s (instance %s) from AWS event %s",
			nodeName, instanceID, eventArn,
		)
	case <-ctx.Done():
		klog.Warningf(
			"Context cancelled while sending event for node %s (instance %s, event %s)",
			nodeName,
			instanceID,
			eventArn,
		)

		return
	}
}

// processAWSHealthEvent handles a single AWS Health event and sends maintenance events for affected instances
func (c *AWSClient) processAWSHealthEvent(
	ctx context.Context,
	eventArn string,
	evt types.Event,
	nodeMap map[string]string,
	eventChan chan<- model.MaintenanceEvent,
) {
	klog.V(3).Infof("Processing AWS Health event with ARN %s", eventArn)

	desc := c.getEventDescription(ctx, evt)
	action := c.mapToValidAction(desc)

	affectedEntities, err := c.getAffectedEntities(ctx, eventArn)
	if err != nil {
		klog.Errorf("Error getting affected entities for event %s: %v",
			eventArn, err)
		return
	}

	// Process all affected entities for this event
	for _, entity := range affectedEntities {
		c.processSingleEntityForEvent(ctx, eventArn, evt, entity, desc, action, nodeMap, eventChan)
	}
}

func (c *AWSClient) getEventDescription(ctx context.Context, event types.Event) string {
	start := time.Now()
	detailedEvents, err := c.awsClient.DescribeEventDetails(ctx, &health.DescribeEventDetailsInput{
		EventArns: []string{*event.Arn},
	})

	metrics.CSPAPIDuration.WithLabelValues(string(model.CSPAWS),
		"describe_event_details").Observe(time.Since(start).Seconds())

	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPAWS), "describe_event_details").Inc()
		klog.Errorf("Error getting event details for event %s: %v",
			*event.Arn, err)

		return ""
	}

	if len(detailedEvents.SuccessfulSet) == 0 {
		klog.Errorf("No event details found for event %s", *event.Arn)

		return ""
	}

	desc := detailedEvents.SuccessfulSet[0].EventDescription.LatestDescription

	return *desc
}

// getAffectedEntities retrieves affected entities for a specific event
func (c *AWSClient) getAffectedEntities(
	ctx context.Context,
	eventARN string,
) ([]types.AffectedEntity, error) {
	klog.V(3).Infof("Fetching affected entities for event %s", eventARN)

	start := time.Now()
	detailedEvents, err := c.awsClient.DescribeAffectedEntities(ctx, &health.DescribeAffectedEntitiesInput{
		Filter: &types.EntityFilter{
			EventArns: []string{eventARN},
		},
	})

	metrics.CSPAPIDuration.WithLabelValues(string(model.CSPAWS), "describe_affected_entities").
		Observe(time.Since(start).Seconds())

	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPAWS), "DescribeAffectedEntities").Inc()
		return nil, fmt.Errorf("error describing affected entities: %w", err)
	}

	if len(detailedEvents.Entities) == 0 {
		klog.V(3).Infof("No affected entities found for event %s", eventARN)
	} else {
		klog.V(3).Infof(
			"Found %d affected entities for event %s",
			len(detailedEvents.Entities), eventARN,
		)
	}

	return detailedEvents.Entities, nil
}

// pollEventsAPI queries the AWS Health API for events within a time range.
func (c *AWSClient) pollEventsAPI(ctx context.Context, startTime time.Time) ([]types.Event, error) {
	pollStart := time.Now()

	klog.V(2).Infof("Polling AWS Health API...")

	events, err := c.awsClient.DescribeEvents(ctx, &health.DescribeEventsInput{
		Filter: &types.EventFilter{
			Services:            []string{"EC2"},
			EventTypeCategories: []types.EventTypeCategory{types.EventTypeCategoryScheduledChange},
			EventTypeCodes:      SupportedEventTypeCodesList,
			Regions:             []string{c.config.Region},
			LastUpdatedTimes: []types.DateTimeRange{
				{
					From: aws.Time(startTime),
				},
			},
		},
	})
	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPAWS), "DescribeEvents_api_error").Inc()

		klog.Errorf("error while fetching maintenance events: %v", err)

		return nil, fmt.Errorf("error while fetching maintenance events: %w", err)
	}

	if len(events.Events) > 0 {
		klog.V(2).Infof("Found %d scheduled maintenance events", len(events.Events))
	} else {
		klog.V(2).Infof("No scheduled maintenance events found.")
	}

	metrics.CSPAPIDuration.WithLabelValues(string(model.CSPAWS), "describe_events").
		Observe(time.Since(pollStart).Seconds())

	return events.Events, nil
}

// GetNodesProviderId returns a list of EC2 instance IDs for the nodes in this cluster
func (c *AWSClient) getClusterInstanceNodeMap(ctx context.Context) (map[string]string, error) {
	klog.V(3).Info("Fetching Kubernetes nodes to derive EC2 instance IDs")

	nodes, err := c.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	instanceIDs := make(map[string]string)

	for _, node := range nodes.Items {
		if node.Spec.ProviderID == "" {
			klog.Infof("Node %s has no providerID", node.Name)
			continue
		}

		// Parse AWS provider ID format: aws:///us-east-1/i-0123456789abcdef0
		if !strings.HasPrefix(node.Spec.ProviderID, "aws:///") {
			klog.Infof("Node %s has non-AWS providerID: %s", node.Name, node.Spec.ProviderID)
			continue
		}

		idPart := strings.TrimPrefix(node.Spec.ProviderID, "aws:///")

		// The ID might include the zone, so we need to extract just the instance ID
		parts := strings.Split(idPart, "/")
		instanceID := parts[len(parts)-1] // Get the last part which should be the instance ID

		// Ensure it's a valid EC2 instance ID (i-xxxxxxxxxxxxxxxxx format)
		if strings.HasPrefix(instanceID, "i-") {
			instanceIDs[instanceID] = node.Name // Store node name as the value for mapping back
			klog.V(2).Infof("Found instance ID %s for node %s", instanceID, node.Name)
		} else {
			klog.Infof("Unexpected instance ID format for node %s: %s", node.Name, instanceID)
		}
	}

	klog.V(2).Infof("Found %d AWS EC2 instances in the cluster", len(instanceIDs))

	return instanceIDs, nil
}

func (c *AWSClient) mapToValidAction(desc string) pb.RecommenedAction {
	// Split the description into individual lines for easier parsing.
	lines := strings.Split(desc, "\n")

	// Iterate over the lines to locate the "What do I need to do" section.
	for idx, line := range lines {
		if strings.Contains(strings.ToLower(line), "what do i need to do") {
			// Consolidate this line and everything after it into a single section.
			section := strings.ToLower(strings.Join(lines[idx:], " "))

			switch {
			case strings.Contains(section, "reset") && strings.Contains(section, "component"):
				return pb.RecommenedAction_COMPONENT_RESET

			case strings.Contains(section, "stop and start") || strings.Contains(section, "reboot"):
				return pb.RecommenedAction_NODE_REBOOT

			case strings.Contains(section, "replace the instance") || strings.Contains(section, "launch a new"):
				return pb.RecommenedAction_COMPONENT_REPLACEMENT

			default:
				metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "map_to_valid_action_error").Inc()
				klog.V(2).Infof(
					"Found suggested action but not able to parse the string into proper format: %s",
					section,
				)

				return pb.RecommenedAction_NONE
			}
		}
	}
	// default action if no recommended action section is found
	return pb.RecommenedAction_NONE
}
