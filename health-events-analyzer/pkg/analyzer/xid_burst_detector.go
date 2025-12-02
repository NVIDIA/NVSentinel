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

package analyzer

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// XidBurstDetectorConfig holds the configurable parameters for the XID burst detector.
// These values should match the MongoDB RepeatedXidError aggregation pipeline configuration.
type XidBurstDetectorConfig struct {
	BurstWindow    time.Duration // Max gap within a burst (default: 10s for tests, 180s for prod)
	StickyWindow   time.Duration // Sticky XID continuation window (default: 20s for tests, 3h for prod)
	LookbackWindow time.Duration // How far back to keep events (default: 5m for tests, 24h for prod)
	BurstThreshold int           // Number of bursts required to trigger (default: 2 for tests, 5 for prod)
}

// XidBurstDetector tracks GPU XID errors per node and detects burst patterns
// This implements the same logic as the MongoDB RepeatedXidError aggregation pipeline
// but in Go code, making it compatible with PostgreSQL.
//
// The detector identifies when the same GPU XID error code appears in multiple
// "bursts" of errors. A burst is defined as a sequence of errors within 3 minutes
// of each other, with special handling for "sticky" XIDs that extend burst windows.
//
// Configuration matches MongoDB pipeline:
// - burstWindow: 180 seconds (3 minutes) - max gap within a burst
// - stickyWindow: 10800 seconds (3 hours) - sticky XID continuation window
// - lookbackWindow: 86400 seconds (24 hours) - how far back to keep events
// - burstThreshold: 5 - number of bursts required to trigger
type XidBurstDetector struct {
	mu             sync.RWMutex
	nodeEvents     map[string]*NodeXidHistory // nodeName -> history
	burstWindow    time.Duration              // 3 minutes - max gap within a burst
	stickyWindow   time.Duration              // 3 hours - sticky XID continuation window
	lookbackWindow time.Duration              // 24 hours - how far back to keep events
	burstThreshold int                        // 5 - number of bursts required to trigger
	stickyXids     map[string]bool            // XIDs that are "sticky" (74, 79, 95, 109, 119)
}

// NodeXidHistory tracks XID error events for a single node
type NodeXidHistory struct {
	events []XidEvent
}

// XidEvent represents a single GPU XID error event
type XidEvent struct {
	timestamp time.Time
	errorCode string
	gpuIDs    []string
}

// Burst represents a group of XID events that occurred close together in time
type Burst struct {
	startTime time.Time
	events    []XidEvent
	xidCodes  map[string]bool
}

// DefaultXidBurstDetectorConfig returns the default configuration matching production values.
// These match the MongoDB RepeatedXidError pipeline in values.yaml.
func DefaultXidBurstDetectorConfig() XidBurstDetectorConfig {
	return XidBurstDetectorConfig{
		BurstWindow:    3 * time.Minute, // 180 seconds
		StickyWindow:   3 * time.Hour,   // 10800 seconds
		LookbackWindow: 24 * time.Hour,  // 86400 seconds
		BurstThreshold: 5,               // 5+ bursts required
	}
}

// NewXidBurstDetector creates a new XID burst detector with default (production) windows.
// For custom configuration, use NewXidBurstDetectorWithConfig.
func NewXidBurstDetector() *XidBurstDetector {
	return NewXidBurstDetectorWithConfig(DefaultXidBurstDetectorConfig())
}

// NewXidBurstDetectorWithConfig creates a new XID burst detector with the specified configuration.
// This allows the detector to match the MongoDB pipeline configuration from the ConfigMap.
func NewXidBurstDetectorWithConfig(cfg XidBurstDetectorConfig) *XidBurstDetector {
	slog.Info("Creating XID burst detector with config",
		"burstWindow", cfg.BurstWindow,
		"stickyWindow", cfg.StickyWindow,
		"lookbackWindow", cfg.LookbackWindow,
		"burstThreshold", cfg.BurstThreshold)

	return &XidBurstDetector{
		nodeEvents:     make(map[string]*NodeXidHistory),
		burstWindow:    cfg.BurstWindow,
		stickyWindow:   cfg.StickyWindow,
		lookbackWindow: cfg.LookbackWindow,
		burstThreshold: cfg.BurstThreshold,
		stickyXids: map[string]bool{
			"74":  true,
			"79":  true,
			"95":  true,
			"109": true,
			"119": true,
		},
	}
}

// ParseXidConfigFromPipeline extracts XID burst detector configuration from the MongoDB
// aggregation pipeline stages. This ensures the Go-based detector uses the same parameters
// as the MongoDB pipeline defined in the ConfigMap.
//
// The function looks for specific patterns in the pipeline stages:
// - Lookback window: "$subtract": [..., N] in the first $match stage
// - Burst window: "$gt": [..., N] in the burstId calculation stage
// - Sticky window: "$lte": [..., N] in the stickyXidWithin check
// - Burst threshold: "$gte": N in the final $match stage
func ParseXidConfigFromPipeline(stages []string) XidBurstDetectorConfig {
	cfg := DefaultXidBurstDetectorConfig()

	for _, stageStr := range stages {
		var stage map[string]interface{}
		if err := json.Unmarshal([]byte(stageStr), &stage); err != nil {
			continue
		}

		parseMatchStage(stage, &cfg)
		parseSetWindowFieldsStage(stage, &cfg)
		parseAddFieldsStage(stage, &cfg)
	}

	return cfg
}

// parseMatchStage extracts lookback window and burst threshold from $match stages
func parseMatchStage(stage map[string]interface{}, cfg *XidBurstDetectorConfig) {
	match, ok := stage["$match"].(map[string]interface{})
	if !ok {
		return
	}

	// Extract lookback window from $expr.$gte
	extractLookbackWindow(match, cfg)

	// Extract burst threshold from count.$gte
	extractBurstThreshold(match, cfg)
}

// extractLookbackWindow extracts the lookback window from a $match.$expr.$gte stage
func extractLookbackWindow(match map[string]interface{}, cfg *XidBurstDetectorConfig) {
	expr, ok := match["$expr"].(map[string]interface{})
	if !ok {
		return
	}

	gte, ok := expr["$gte"].([]interface{})
	if !ok || len(gte) != 2 {
		return
	}

	subtract, ok := gte[1].(map[string]interface{})
	if !ok {
		return
	}

	sub, ok := subtract["$subtract"].([]interface{})
	if !ok || len(sub) != 2 {
		return
	}

	if seconds, ok := sub[1].(float64); ok {
		cfg.LookbackWindow = time.Duration(seconds) * time.Second
		slog.Debug("Parsed lookback window from pipeline", "seconds", seconds)
	}
}

// extractBurstThreshold extracts the burst threshold from a $match.count.$gte stage
func extractBurstThreshold(match map[string]interface{}, cfg *XidBurstDetectorConfig) {
	count, ok := match["count"].(map[string]interface{})
	if !ok {
		return
	}

	if gte, ok := count["$gte"].(float64); ok {
		cfg.BurstThreshold = int(gte)
		slog.Debug("Parsed burst threshold from pipeline", "threshold", cfg.BurstThreshold)
	}
}

// parseSetWindowFieldsStage extracts burst window from $setWindowFields stages
func parseSetWindowFieldsStage(stage map[string]interface{}, cfg *XidBurstDetectorConfig) {
	swf, ok := stage["$setWindowFields"].(map[string]interface{})
	if !ok {
		return
	}

	output, ok := swf["output"].(map[string]interface{})
	if !ok {
		return
	}

	burstID, ok := output["burstId"].(map[string]interface{})
	if !ok {
		return
	}

	if window := findGtInValue(burstID); window > 0 {
		cfg.BurstWindow = window
		slog.Debug("Parsed burst window from pipeline", "duration", window)
	}
}

// parseAddFieldsStage extracts sticky window from $addFields stages
func parseAddFieldsStage(stage map[string]interface{}, cfg *XidBurstDetectorConfig) {
	addFields, ok := stage["$addFields"].(map[string]interface{})
	if !ok {
		return
	}

	for key, value := range addFields {
		if key == "stickyXidWithin3Hours" || key == "stickyXidWithin20s" {
			if window := findLteInValue(value); window > 0 {
				cfg.StickyWindow = window
				slog.Debug("Parsed sticky window from pipeline", "duration", window)
			}
		}
	}
}

// findGtInValue recursively searches for $gt operator and extracts the time value
func findGtInValue(v interface{}) time.Duration {
	switch val := v.(type) {
	case map[string]interface{}:
		if result := checkGtOperator(val); result > 0 {
			return result
		}

		for _, child := range val {
			if result := findGtInValue(child); result > 0 {
				return result
			}
		}
	case []interface{}:
		for _, child := range val {
			if result := findGtInValue(child); result > 0 {
				return result
			}
		}
	}

	return 0
}

// checkGtOperator checks if a map contains a $gt operator with a time value
func checkGtOperator(val map[string]interface{}) time.Duration {
	gt, ok := val["$gt"].([]interface{})
	if !ok || len(gt) != 2 {
		return 0
	}

	subtract, ok := gt[0].(map[string]interface{})
	if !ok {
		return 0
	}

	if _, ok := subtract["$subtract"]; !ok {
		return 0
	}

	if seconds, ok := gt[1].(float64); ok {
		return time.Duration(seconds) * time.Second
	}

	return 0
}

// findLteInValue recursively searches for $lte operator and extracts the time value
func findLteInValue(v interface{}) time.Duration {
	switch val := v.(type) {
	case map[string]interface{}:
		if result := checkLteOperator(val); result > 0 {
			return result
		}

		for _, child := range val {
			if result := findLteInValue(child); result > 0 {
				return result
			}
		}
	case []interface{}:
		for _, child := range val {
			if result := findLteInValue(child); result > 0 {
				return result
			}
		}
	}

	return 0
}

// checkLteOperator checks if a map contains a $lte operator with a time value
func checkLteOperator(val map[string]interface{}) time.Duration {
	lte, ok := val["$lte"].([]interface{})
	if !ok || len(lte) != 2 {
		return 0
	}

	if seconds, ok := lte[1].(float64); ok {
		return time.Duration(seconds) * time.Second
	}

	return 0
}

// ProcessEvent analyzes a new XID error event and determines if it should trigger
// a RepeatedXidError alert. Returns true if the same XID appears in burstThreshold+ bursts.
func (d *XidBurstDetector) ProcessEvent(event *protos.HealthEvent) (shouldTrigger bool, burstCount int) {
	d.mu.Lock()
	defer d.mu.Unlock()

	nodeName := event.NodeName
	if len(event.ErrorCode) == 0 {
		return false, 0
	}

	xidCode := event.ErrorCode[0]
	timestamp := time.Unix(event.GeneratedTimestamp.Seconds, 0)

	// Get or create node history
	history := d.getOrCreateHistory(nodeName)

	// Add new event
	xidEvent := XidEvent{
		timestamp: timestamp,
		errorCode: xidCode,
		gpuIDs:    extractGPUIDs(event.EntitiesImpacted),
	}
	history.events = append(history.events, xidEvent)

	// Clean up old events (older than lookback window)
	d.cleanupOldEvents(history, timestamp)

	// Detect bursts using same logic as MongoDB pipeline
	bursts := d.detectBursts(history, xidCode)

	// Trigger if we see the same XID code in burstThreshold+ different bursts
	// Default threshold is 5, matching MongoDB pipeline
	return len(bursts) >= d.burstThreshold, len(bursts)
}

// detectBursts identifies burst patterns in the event history for a specific XID code
// A burst is a sequence of events where consecutive events are within burstWindow of each other
// Special handling: sticky XIDs can extend bursts if they occur within stickyWindow
func (d *XidBurstDetector) detectBursts(history *NodeXidHistory, targetXid string) []Burst {
	if len(history.events) == 0 {
		return nil
	}

	var bursts []Burst

	currentBurst := &Burst{
		startTime: history.events[0].timestamp,
		xidCodes:  make(map[string]bool),
	}

	for i, event := range history.events {
		// Check if this event starts a new burst
		if i > 0 {
			if d.shouldStartNewBurst(event, history.events[i-1], history.events[:i]) {
				// Save current burst if it contains the target XID
				if currentBurst.hasXid(targetXid) {
					bursts = append(bursts, *currentBurst)
				}

				// Start new burst
				currentBurst = &Burst{
					startTime: event.timestamp,
					xidCodes:  make(map[string]bool),
				}
			}
		}

		currentBurst.addEvent(event)
	}

	// Add final burst if it contains the target XID
	if currentBurst.hasXid(targetXid) {
		bursts = append(bursts, *currentBurst)
	}

	return bursts
}

// shouldStartNewBurst determines if an event should start a new burst
// based on time gap and sticky XID continuation logic
func (d *XidBurstDetector) shouldStartNewBurst(
	event XidEvent,
	prevEvent XidEvent,
	previousEvents []XidEvent,
) bool {
	timeDiff := event.timestamp.Sub(prevEvent.timestamp)

	// New burst if gap > burstWindow (10 seconds)
	if timeDiff > d.burstWindow {
		// Unless it's a sticky XID within stickyWindow (20 seconds) of a previous sticky XID
		isStickyContinuation := d.isStickyXidContinuation(event, previousEvents)

		return !isStickyContinuation
	}

	return false
}

// isStickyXidContinuation checks if a sticky XID should extend the current burst
// Returns true if there's another sticky XID within stickyWindow before this event
func (d *XidBurstDetector) isStickyXidContinuation(event XidEvent, previousEvents []XidEvent) bool {
	if !d.stickyXids[event.errorCode] {
		return false
	}

	// Check if there's a sticky XID within stickyWindow (20 seconds) before this one
	for i := len(previousEvents) - 1; i >= 0; i-- {
		prev := previousEvents[i]
		timeDiff := event.timestamp.Sub(prev.timestamp)

		if timeDiff > d.stickyWindow {
			break // Too far back, no sticky continuation
		}

		if d.stickyXids[prev.errorCode] {
			return true // Found sticky XID within window
		}
	}

	return false
}

// addEvent adds an event to the burst and tracks its XID code
func (b *Burst) addEvent(event XidEvent) {
	b.events = append(b.events, event)
	if b.xidCodes == nil {
		b.xidCodes = make(map[string]bool)
	}

	b.xidCodes[event.errorCode] = true
}

// hasXid checks if the burst contains a specific XID code
func (b *Burst) hasXid(xidCode string) bool {
	return b.xidCodes[xidCode]
}

// extractGPUIDs extracts GPU entity values from the entities impacted list
func extractGPUIDs(entities []*protos.Entity) []string {
	var gpuIDs []string

	for _, entity := range entities {
		if entity.EntityType == "GPU" {
			gpuIDs = append(gpuIDs, entity.EntityValue)
		}
	}

	return gpuIDs
}

// cleanupOldEvents removes events older than the lookback window
func (d *XidBurstDetector) cleanupOldEvents(history *NodeXidHistory, currentTime time.Time) {
	cutoff := currentTime.Add(-d.lookbackWindow)
	validEvents := make([]XidEvent, 0, len(history.events))

	for _, event := range history.events {
		if event.timestamp.After(cutoff) {
			validEvents = append(validEvents, event)
		}
	}

	history.events = validEvents
}

// getOrCreateHistory gets the event history for a node, creating it if it doesn't exist
func (d *XidBurstDetector) getOrCreateHistory(nodeName string) *NodeXidHistory {
	if history, exists := d.nodeEvents[nodeName]; exists {
		return history
	}

	history := &NodeXidHistory{events: make([]XidEvent, 0)}
	d.nodeEvents[nodeName] = history

	return history
}

// GetBurstStats returns statistics about current burst tracking (for observability)
func (d *XidBurstDetector) GetBurstStats() map[string]int {
	d.mu.RLock()
	defer d.mu.RUnlock()

	stats := make(map[string]int)
	for nodeName, history := range d.nodeEvents {
		stats[nodeName] = len(history.events)
	}

	return stats
}

// ClearNodeHistory clears all XID event history for a specific node.
// This should be called when a healthy event is received for the node,
// indicating that the GPU issues have been resolved and we should start fresh.
func (d *XidBurstDetector) ClearNodeHistory(nodeName string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.nodeEvents, nodeName)
}
