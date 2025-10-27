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

package helpers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// SendHealthEventsToNodes sends health events from the specified `eventFilePath` to all nodes listed in `nodeNames` concurrently.
func SendHealthEventsToNodes(nodeNames []string, eventFilePath string) error {
	eventData, err := os.ReadFile(eventFilePath)
	if err != nil {
		return fmt.Errorf("failed to read health event file %s: %w", eventFilePath, err)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, nodeName := range nodeNames {
		wg.Add(1)
		go func(nodeName string) {
			defer wg.Done()

			eventJSON := strings.ReplaceAll(string(eventData), "NODE_NAME", nodeName)

			resp, err := client.Post("http://localhost:8080/health-event", "application/json", strings.NewReader(eventJSON))
			if err != nil {
				mu.Lock()
				defer mu.Unlock()
				errs = append(errs, fmt.Errorf("failed to send health event to node %s: %w", nodeName, err))
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				mu.Lock()
				defer mu.Unlock()
				errs = append(errs, fmt.Errorf("health event to node %s failed: expected status 200, got %d. Response: %s", nodeName, resp.StatusCode, string(body)))
			}
		}(nodeName)
	}

	wg.Wait()

	return errors.Join(errs...)
}
