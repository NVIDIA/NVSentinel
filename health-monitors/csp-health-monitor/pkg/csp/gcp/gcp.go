// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package gcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	klog "k8s.io/klog/v2"
)

const (
	gcpInstanceIDAnnotation = "container.googleapis.com/instance_id"
	gcpInstanceIDUnknown    = "UNKNOWN"
)

// EntryIterator defines the interface for iterating over log entries. It's
// compatible with *logadmin.EntryIterator. This allows for mocking in tests.
// *logging.Entry is from cloud.google.com/go/logging
type EntryIterator interface {
	Next() (*logging.Entry, error)
}

// LogAdminAPI defines the interface for the logadmin client methods used by our
// gcp.Client. This allows for mocking in tests. *logadmin.EntryIterator is from
// cloud.google.com/go/logging/logadmin
type LogAdminAPI interface {
	Entries(ctx context.Context, opts ...logadmin.EntriesOption) *logadmin.EntryIterator
	Close() error
}

// Client encapsulates all state required to poll GCP Cloud Logging for
// maintenance events and forward them to the main pipeline.
type Client struct {
	config                               config.GCPConfig
	logadminClient                       LogAdminAPI
	k8sClientset                         kubernetes.Interface
	normalizer                           eventpkg.Normalizer
	clusterName                          string
	kubeconfigPath                       string
	lastSuccessfullyProcessedPollEndTime time.Time
}

// getInitialPollStartTime determines the starting point for polling GCP logs.
// It prioritizes the last processed timestamp from the datastore, falling back
// to the current time.
func getInitialPollStartTime(
	ctx context.Context,
	store datastore.Store,
	clusterName string,
	nowUTC time.Time,
) time.Time {
	if store == nil {
		klog.Warningf("Datastore client is nil for GCP monitor. Starting poll from current time.")

		return nowUTC
	}

	lastProcessedEventTS, found, errDb := store.GetLastProcessedEventTimestampByCSP(
		ctx,
		clusterName,
		model.CSPGCP,
		"GCP",
	)
	if errDb != nil {
		klog.Warningf(
			"Failed to get last processed GCP log event timestamp for cluster %s from datastore: %v. "+
				"Starting poll from current time.",
			clusterName,
			errDb,
		)

		return nowUTC
	}

	if found && !lastProcessedEventTS.IsZero() {
		klog.Infof(
			"Resuming poll: last processed GCP log event timestamp for cluster %s is %v. "+
				"Next poll window will start after this.",
			clusterName,
			lastProcessedEventTS.Format(time.RFC3339Nano),
		)

		return lastProcessedEventTS
	}

	klog.Infof(
		"No previous GCP logs checkpoint found in datastore for cluster %s. "+
			"Starting poll from current time.",
		clusterName,
	)

	return nowUTC
}

// NewClient builds and initialises a new GCP monitoring Client.
//
// It creates:
//   - a Cloud Logging admin client
//   - a Kubernetes clientset
//   - a normalizer for GCP-specific log entries
//
// It also determines the checkpoint timestamp from the datastore (if provided)
// so that polling resumes where it left off.
func NewClient(
	ctx context.Context,
	cfg config.GCPConfig,
	clusterName string,
	kubeconfigPath string,
	store datastore.Store,
) (*Client, error) {
	if !cfg.Enabled {
		klog.InfoS("GCP Client: Monitoring is disabled in configuration. Client initialization aborted.")
		return nil, fmt.Errorf("GCP monitoring is disabled by configuration, client not created")
	}

	normalizer, err := eventpkg.GetNormalizer(model.CSPGCP)
	if err != nil {
		return nil, fmt.Errorf("failed to get GCP normalizer: %w", err)
	}

	opts := []option.ClientOption{option.WithScopes(logging.ReadScope)}

	adminClient, err := logadmin.NewClient(ctx, cfg.TargetProjectID, opts...)
	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPGCP), "logadmin_client_init").Inc()
		return nil, fmt.Errorf("failed to create logadmin client for project %s: %w", cfg.TargetProjectID, err)
	}

	var k8sClient kubernetes.Interface

	var k8sConfig *rest.Config

	if kubeconfigPath != "" {
		klog.Infof("GCP Client: Using kubeconfig from path: %s", kubeconfigPath)
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		klog.Info("GCP Client: KubeconfigPath not specified, attempting in-cluster config.")

		k8sConfig, err = rest.InClusterConfig()
	}

	if err != nil {
		metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPGCP), "k8s_config_error").Inc()

		return nil, fmt.Errorf(
			"GCP client (enabled) failed to initialize K8s config (kubeconfig: '%s'): %w",
			kubeconfigPath,
			err,
		)
	}

	k8sClient, err = kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPGCP), "k8s_clientset_error").Inc()
		return nil, fmt.Errorf("GCP client (enabled) failed to create K8s clientset: %w", err)
	}

	klog.Info("GCP Client: Kubernetes clientset initialized successfully.")

	initialPollStartTime := getInitialPollStartTime(ctx, store, clusterName, time.Now().UTC())
	klog.Infof(
		"GCP poller initial 'lastSuccessfullyProcessedPollEndTime' set to: %s",
		initialPollStartTime.Format(time.RFC3339Nano),
	)

	return &Client{
		config:                               cfg,
		logadminClient:                       adminClient,
		k8sClientset:                         k8sClient,
		normalizer:                           normalizer,
		clusterName:                          clusterName,
		kubeconfigPath:                       kubeconfigPath,
		lastSuccessfullyProcessedPollEndTime: initialPollStartTime,
	}, nil
}

// mapGCPInstanceToNodeName attempts to translate a GCP numeric instance ID to a
// Kubernetes Node by scanning annotations. It returns "" when no match is found
// or when required data is missing.
func (c *Client) mapGCPInstanceToNodeName(
	ctx context.Context,
	gcpNumericInstanceID string,
	eventMetadata map[string]string,
) (string, error) {
	if gcpNumericInstanceID == "" || gcpNumericInstanceID == gcpInstanceIDUnknown {
		klog.V(2).Info("GCP mapGCPInstanceToNodeName: cannot map empty or UNKNOWN gcpNumericInstanceID.")
		return "", nil
	}

	listOptions := metav1.ListOptions{}
	zone, zoneProvided := eventMetadata["gcp_zone"]

	if zoneProvided && zone != "" {
		listOptions.LabelSelector = fmt.Sprintf("topology.kubernetes.io/zone=%s", zone)
		klog.V(3).Infof("GCP mapGCPInstanceToNodeName: Applying label selector for zone: %s", listOptions.LabelSelector)
	} else {
		klog.V(2).Info("GCP mapGCPInstanceToNodeName: Zone not available in event metadata, " +
			"listing all nodes for mapping. This might be slow.")
	}

	nodes, listErr := c.k8sClientset.CoreV1().Nodes().List(ctx, listOptions)
	if listErr != nil {
		return "", fmt.Errorf(
			"failed to list K8s nodes (selector='%s') for GCP mapping: %w",
			listOptions.LabelSelector,
			listErr,
		)
	}

	klog.V(3).Infof(
		"GCP mapGCPInstanceToNodeName: Listed %d nodes (selector '%s'), searching for match for instance ID '%s'",
		len(nodes.Items),
		listOptions.LabelSelector,
		gcpNumericInstanceID,
	)

	for _, node := range nodes.Items {
		if numericIDFromAnnotation, ok := node.Annotations[gcpInstanceIDAnnotation]; ok {
			if numericIDFromAnnotation == gcpNumericInstanceID {
				klog.V(1).Infof(
					"GCP mapGCPInstanceToNodeName: Found K8s Node '%s' by matching annotation '%s' (value: '%s') "+
						"with event resource ID '%s'",
					node.Name, gcpInstanceIDAnnotation, numericIDFromAnnotation, gcpNumericInstanceID)

				return node.Name, nil
			}
		}
	}

	klog.V(1).Infof(
		"GCP mapGCPInstanceToNodeName: No Kubernetes node found matching GCP numeric instance ID '%s' "+
			"(Zone: '%s') after checking %d listed nodes.",
		gcpNumericInstanceID,
		zone,
		len(nodes.Items),
	)

	return "", nil
}

func (c *Client) GetName() model.CSP {
	return model.CSPGCP
}

// StartMonitoring launches the periodic log-polling loop and streams normalized
// maintenance events to eventChan until the context is cancelled.
func (c *Client) StartMonitoring(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error {
	// If NewClient succeeded, c.config.Enabled is true.
	klog.InfoS("Starting GCP Cloud Logging API poller",
		"project", c.config.TargetProjectID,
		"intervalSeconds", c.config.APIPollingIntervalSeconds,
		"effectiveInitialQueryStartTime", c.lastSuccessfullyProcessedPollEndTime.Format(time.RFC3339Nano),
		"filter", c.config.LogFilter)

	// Perform initial poll immediately.
	// Check context before the initial poll, in case it was cancelled very quickly.
	if ctx.Err() == nil {
		c.pollLogs(ctx, eventChan)
	} else {
		klog.InfoS("GCP API monitoring not starting initial poll due to context cancellation.")

		if err := c.logadminClient.Close(); err != nil { // logadminClient guaranteed non-nil by NewClient
			klog.ErrorS(err, "Error closing logadmin client during early context cancellation")
		}

		return ctx.Err()
	}

	ticker := time.NewTicker(time.Duration(c.config.APIPollingIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.InfoS("GCP API monitoring stopping due to context cancellation.")

			if err := c.logadminClient.Close(); err != nil {
				klog.ErrorS(err, "Error closing logadmin client")
			}

			return ctx.Err()
		case <-ticker.C:
			c.pollLogs(ctx, eventChan)
		}
	}
}

// pollLogs executes a single polling cycle against the Cloud Logging API. It is
// invoked periodically from StartMonitoring.
func (c *Client) pollLogs(ctx context.Context, eventChan chan<- model.MaintenanceEvent) {
	queryWindowStartTime := c.lastSuccessfullyProcessedPollEndTime
	queryWindowEndTime := time.Now().UTC()

	if !queryWindowStartTime.Before(queryWindowEndTime) {
		klog.V(1).Infof(
			"GCP Query window start time (%s) is not before query window end time (%s). "+
				"Adjusting end time for query validity.",
			queryWindowStartTime.Format(time.RFC3339Nano),
			queryWindowEndTime.Format(time.RFC3339Nano),
		)

		if !queryWindowEndTime.After(queryWindowStartTime) {
			queryWindowEndTime = queryWindowStartTime.Add(1 * time.Millisecond)
		}
	}

	klog.V(1).InfoS("Polling GCP Logging API...",
		"queryWindowStartExclusive", queryWindowStartTime.Format(time.RFC3339Nano),
		"queryWindowEndInclusive", queryWindowEndTime.Format(time.RFC3339Nano))

	apiPollStartTs := time.Now()
	timeFilter := fmt.Sprintf("timestamp > \"%s\" AND timestamp <= \"%s\"",
		queryWindowStartTime.Format(time.RFC3339Nano),
		queryWindowEndTime.Format(time.RFC3339Nano))
	fullFilter := fmt.Sprintf("(%s) AND (%s)", c.config.LogFilter, timeFilter)

	klog.V(2).InfoS("Executing GCP log query", "filter", fullFilter)

	it := c.logadminClient.Entries(ctx, logadmin.Filter(fullFilter))

	pollFetchSuccessful := c.processLogEntries(
		ctx,
		it,
		queryWindowEndTime,
		eventChan,
		apiPollStartTs,
	)

	if pollFetchSuccessful {
		c.lastSuccessfullyProcessedPollEndTime = queryWindowEndTime
	} else {
		klog.Warningf(
			"GCP poll cycle encountered errors fetching or processing entries. Checkpoint "+
				"NOT advanced. durationSec=%.2f lastSuccessfullyProcessedPollEndTimeNotUpdated=%s",
			time.Since(apiPollStartTs).Seconds(),
			c.lastSuccessfullyProcessedPollEndTime.Format(time.RFC3339Nano),
		)
	}
}

// getNodeNameForGcpLogEntry attempts to map a GCP log entry to a Kubernetes
// node name.
func (c *Client) getNodeNameForGcpLogEntry(
	ctx context.Context,
	gcpNumericInstanceID string,
	gcpZone string,
	eventMetaForMapper map[string]string,
) (string, error) {
	var nodeName string

	var mappingErr error

	if gcpNumericInstanceID == "" || gcpNumericInstanceID == gcpInstanceIDUnknown {
		klog.V(2).Infof("GCP instance ID ('%s') is empty or UNKNOWN from log entry, skipping K8s node name mapping.",
			gcpNumericInstanceID)
	} else {
		klog.V(3).Infof("Attempting K8s mapping for GCP Instance ID: %s, Zone: %s", gcpNumericInstanceID, gcpZone)

		nodeName, mappingErr = c.mapGCPInstanceToNodeName(ctx, gcpNumericInstanceID, eventMetaForMapper)
		//nolint:gocritic
		if mappingErr != nil {
			klog.Warningf(
				"Error mapping GCP resource ID '%s' (Zone: '%s') to K8s node name: %v. "+
					"Proceeding without node name.",
				gcpNumericInstanceID,
				gcpZone,
				mappingErr,
			)
		} else if nodeName == "" {
			klog.V(1).Infof(
				"No K8s node found for GCP resource ID '%s' (Numeric ID, Zone: '%s'). "+
					"Event will be processed without NodeName. Treating as an error.",
				gcpNumericInstanceID,
				gcpZone,
			)

			mappingErr = fmt.Errorf("no K8s node found for GCP numeric instance ID '%s' (Zone: '%s')",
				gcpNumericInstanceID, gcpZone)
		} else {
			klog.V(1).Infof(
				"Mapped GCP resource ID '%s' (Numeric ID, Zone: '%s') to K8s Node: %s",
				gcpNumericInstanceID,
				gcpZone,
				nodeName,
			)
		}
	}

	return nodeName, mappingErr
}

// processSingleGcpLogEntry handles the processing of a single log entry from
// GCP.
func (c *Client) processSingleGcpLogEntry(
	ctx context.Context,
	entry *logging.Entry,
	eventChan chan<- model.MaintenanceEvent,
) {
	metrics.CSPEventsReceived.WithLabelValues(string(model.CSPGCP)).Inc()
	klog.V(2).InfoS(
		"Raw GCP Log Entry received",
		"InsertID", entry.InsertID,
		"LogName", entry.LogName,
		"Timestamp", entry.Timestamp.Format(time.RFC3339Nano),
	)

	gcpNumericInstanceID := ""
	gcpZone := ""
	eventMetaForMapper := make(map[string]string)

	if entry.Resource != nil && entry.Resource.Type == "gce_instance" {
		gcpNumericInstanceID = entry.Resource.Labels["instance_id"]

		gcpZone = entry.Resource.Labels["zone"]
		if gcpZone != "" {
			eventMetaForMapper["gcp_zone"] = gcpZone
		}
	}

	nodeName, _ := c.getNodeNameForGcpLogEntry(ctx, gcpNumericInstanceID, gcpZone, eventMetaForMapper)
	// We proceed even if mappingErr is not nil, as nodeName will be empty and
	// handled by normalizer.

	metrics.MainEventsToNormalize.WithLabelValues(string(model.CSPGCP)).Inc()

	normalizedEvent, errNorm := c.normalizer.Normalize(entry, nodeName, c.clusterName)
	if errNorm != nil {
		metrics.MainNormalizationErrors.WithLabelValues(string(model.CSPGCP)).Inc()
		klog.ErrorS(errNorm, "Error normalizing GCP log entry", "InsertID", entry.InsertID)

		return // Skip this event
	}

	select {
	case eventChan <- *normalizedEvent:
		klog.V(2).InfoS(
			"Sent normalized GCP event to channel",
			"eventID", normalizedEvent.EventID,
			"nodeName", normalizedEvent.NodeName,
			"cspStatus", normalizedEvent.CSPStatus,
			"internalStatus", normalizedEvent.Status,
		)
	case <-ctx.Done():
		klog.Info("Context cancelled while sending GCP event to channel. Entry processing stopped for this event.")
	}
}

// processLogEntries iterates through log entries, normalizes them, and sends
// them to the event channel. It returns true if log fetching was successful
// (even if normalization or sending failed for some entries), and false if
// fetching logs itself failed (e.g., iterator.Next() returned a non-Done
// error). apiPollStartTs is passed for consistent duration calculation.
func (c *Client) processLogEntries(
	ctx context.Context,
	it EntryIterator,
	queryWindowEndTime time.Time,
	eventChan chan<- model.MaintenanceEvent,
	apiPollStartTs time.Time,
) (pollFetchSuccessful bool) {
	entriesProcessedThisPoll := 0
	pollFetchSuccessful = true

	var latestTimestampInBatch time.Time

	for {
		// Check for context cancellation at the start of each iteration
		if err := ctx.Err(); err != nil {
			klog.Infof("Context cancelled before processing next entry: %v", err)

			pollFetchSuccessful = false // Ensure this is false if loop is exited due to cancellation

			return pollFetchSuccessful
		}

		entry, err := it.Next()

		if errors.Is(err, iterator.Done) {
			break
		}

		if err != nil {
			klog.ErrorS(
				err,
				"Error iterating GCP log entries",
				"filter",
				c.config.LogFilter,
			) // Use c.config.LogFilter for context
			metrics.CSPAPIErrors.WithLabelValues(string(model.CSPGCP), "iteration").Inc()

			pollFetchSuccessful = false

			break
		}

		entriesProcessedThisPoll++

		if entry.Timestamp.After(latestTimestampInBatch) {
			latestTimestampInBatch = entry.Timestamp
		}

		c.processSingleGcpLogEntry(ctx, entry, eventChan)

		// Check context after processing each entry to allow quick exit if
		// needed
		if ctx.Err() != nil {
			klog.Info("Context cancelled after processing an entry. Stopping poll.")

			pollFetchSuccessful = false

			return pollFetchSuccessful
		}
	}

	pollDuration := time.Since(apiPollStartTs).Seconds()
	metrics.CSPPollingDuration.WithLabelValues(string(model.CSPGCP)).Observe(pollDuration)

	if pollFetchSuccessful {
		if entriesProcessedThisPoll > 0 {
			klog.V(1).InfoS(
				"GCP poll cycle finished.",
				"entriesProcessed", entriesProcessedThisPoll,
				"durationSec", pollDuration,
				"queryWindowUsedEndedAt", queryWindowEndTime.Format(time.RFC3339Nano),
				"latestEventInBatchTimestamp", latestTimestampInBatch.Format(time.RFC3339Nano),
			)
		} else {
			klog.V(1).InfoS(
				"No new GCP log entries found in this poll cycle.",
				"durationSec", pollDuration,
				"queryWindowUsedEndedAt", queryWindowEndTime.Format(time.RFC3339Nano),
			)
		}
	}

	return pollFetchSuccessful
}
