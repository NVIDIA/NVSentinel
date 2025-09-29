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

package main

import (
	"log"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-event-client/pkg"
)

func main() {
	// Log version for debugging
	log.Printf("=== HEALTH EVENT CLIENT STARTING ===")

	// Load configuration
	config, err := pkg.LoadConfig()
	if err != nil {
		log.Printf("Failed to load configuration: %v", err)
		return
	}

	// Create health event manager
	manager, err := pkg.NewHealthEventManager(config.SocketPath)
	if err != nil {
		log.Printf("Failed to create health event manager: %v", err)
		return
	}

	defer func() {
		if err := manager.Close(); err != nil {
			log.Printf("Warning: failed to close manager: %v", err)
		}
	}()

	if config.EventID != "" {
		// Monitor specific event
		if err := manager.MonitorEvent(config.EventID); err != nil {
			log.Printf("Failed to monitor event: %v", err)
			return
		}
	} else {
		// Process health event
		if err := manager.ProcessHealthEvent(config); err != nil {
			log.Printf("Failed to process health event: %v", err)
			return
		}
	}
}
