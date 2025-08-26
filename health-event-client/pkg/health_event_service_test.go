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
	"testing"
)

func TestCreateHealthEvent(t *testing.T) {
	// Create a test manager
	service := &HealthEventManager{}

	// Test configuration
	config := &Config{
		NodeName:          "test-node",
		ErrorCode:         "TEST_ERROR",
		Reason:            "Test reason",
		IsHealthy:         false,
		RecommendedAction: 1,
		CreatorID:         "test-creator",
		Force:             false,
		SkipQuarantine:    false,
		SkipDrain:         false,
		SocketPath:        "/tmp/test.sock",
	}

	// Test creating health event
	healthEvent, err := service.CreateHealthEvent(config)
	if err != nil {
		t.Fatalf("Failed to create health event: %v", err)
	}

	// Verify the health event fields
	if healthEvent.NodeName != config.NodeName {
		t.Errorf("Expected NodeName %s, got %s", config.NodeName, healthEvent.NodeName)
	}

	if healthEvent.CheckName != config.ErrorCode {
		t.Errorf("Expected CheckName %s, got %s", config.ErrorCode, healthEvent.CheckName)
	}

	if healthEvent.IsHealthy != config.IsHealthy {
		t.Errorf("Expected IsHealthy %t, got %t", config.IsHealthy, healthEvent.IsHealthy)
	}

	if healthEvent.RecommendedAction != 1 {
		t.Errorf("Expected RecommendedAction 1, got %d", healthEvent.RecommendedAction)
	}

	// Verify quarantine overrides
	if !healthEvent.QuarantineOverrides.Force {
		t.Error("Expected QuarantineOverrides.Force to be true")
	}

	if healthEvent.QuarantineOverrides.Skip != config.SkipQuarantine {
		t.Errorf("Expected QuarantineOverrides.Skip %t, got %t", config.SkipQuarantine, healthEvent.QuarantineOverrides.Skip)
	}

	// Verify drain overrides
	if healthEvent.DrainOverrides.Force != config.Force {
		t.Errorf("Expected DrainOverrides.Force %t, got %t", config.Force, healthEvent.DrainOverrides.Force)
	}

	if healthEvent.DrainOverrides.Skip != config.SkipDrain {
		t.Errorf("Expected DrainOverrides.Skip %t, got %t", config.SkipDrain, healthEvent.DrainOverrides.Skip)
	}
}

func TestCreateHealthEventWithForce(t *testing.T) {
	service := &HealthEventManager{}

	config := &Config{
		NodeName:          "test-node",
		ErrorCode:         "TEST_ERROR",
		Reason:            "Test reason",
		IsHealthy:         false,
		RecommendedAction: 1,
		CreatorID:         "test-creator",
		Force:             true, // Test force flag
		SkipQuarantine:    false,
		SkipDrain:         false,
		SocketPath:        "/tmp/test.sock",
	}

	healthEvent, err := service.CreateHealthEvent(config)
	if err != nil {
		t.Fatalf("Failed to create health event: %v", err)
	}

	// Verify force flag affects drain overrides
	if !healthEvent.DrainOverrides.Force {
		t.Error("Expected DrainOverrides.Force to be true when Force is true")
	}
}


