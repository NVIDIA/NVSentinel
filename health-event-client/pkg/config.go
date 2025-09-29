/*
Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pkg

import (
	"flag"
	"fmt"
	"math"
)

// Config holds all the configuration options
type Config struct {
	NodeName          string `json:"node_name"`
	ErrorCode         string `json:"error_code"`
	Reason            string `json:"reason"`
	IsHealthy         bool   `json:"is_healthy"`
	RecommendedAction int32  `json:"recommended_action"`
	CreatorID         string `json:"creator_id"`
	Force             bool   `json:"force"`
	SkipQuarantine    bool   `json:"skip_quarantine"`
	SkipDrain         bool   `json:"skip_drain"`
	SocketPath        string `json:"socket_path"`
	EventID           string `json:"event_id"`
}

// LoadConfig loads configuration from command line flags
func LoadConfig() (*Config, error) {
	// Parse command line flags
	flags, err := parseFlags()
	if err != nil {
		return nil, err
	}

	// Create config from CLI flags
	config := &Config{
		NodeName:          flags.nodeName,
		ErrorCode:         flags.errorCode,
		Reason:            flags.reason,
		Force:             flags.force,
		IsHealthy:         flags.isHealthy,
		SkipQuarantine:    flags.skipQuarantine,
		SkipDrain:         flags.skipDrain,
		RecommendedAction: flags.recommendedAction,
		CreatorID:         flags.creatorID,
		SocketPath:        flags.socketPath,
		EventID:           flags.eventID,
	}

	return config, nil
}

// flagValues holds the parsed command line flag values
type flagValues struct {
	nodeName          string
	errorCode         string
	reason            string
	force             bool
	isHealthy         bool
	skipQuarantine    bool
	skipDrain         bool
	creatorID         string
	socketPath        string
	recommendedAction int32
	eventID           string
}

// parseFlags defines and parses command line flags
func parseFlags() (*flagValues, error) {
	// Define flags
	nodeName := flag.String("node-name", "", "Name of the DGX node (required)")
	errorCode := flag.String("error-code", "", "Error code for the health event (required)")
	reason := flag.String("reason", "", "Reason for the operation (required)")
	force := flag.Bool("force", false, "Force the operation")
	isHealthy := flag.Bool("is-healthy", false, "Is healthy event (required)")
	skipQuarantine := flag.Bool("skip-quarantine", false, "Skip quarantine")
	skipDrain := flag.Bool("skip-drain", false, "Skip drain")
	creatorID := flag.String("creator-id", "default", "Creator ID (required)")
	socketPath := flag.String("socket", "/var/run/nvsentinel/nvsentinel.sock", "Platform connector socket path")
	recommendedActionFlag := flag.Int64("recommended-action", 1, "Recommended action (required)")
	eventID := flag.String("event-id", "", "Optional eventID for monitoring specific events")

	flag.Parse()

	// Validate user input after parsing flags
	flagRecommendedAction := *recommendedActionFlag

	if flagRecommendedAction < math.MinInt32 || flagRecommendedAction > math.MaxInt32 {
		return nil, fmt.Errorf("recommended action value is out of int32 bounds")
	}

	return &flagValues{
		nodeName:          *nodeName,
		errorCode:         *errorCode,
		reason:            *reason,
		force:             *force,
		isHealthy:         *isHealthy,
		skipQuarantine:    *skipQuarantine,
		skipDrain:         *skipDrain,
		creatorID:         *creatorID,
		socketPath:        *socketPath,
		recommendedAction: int32(flagRecommendedAction),
		eventID:           *eventID,
	}, nil
}
