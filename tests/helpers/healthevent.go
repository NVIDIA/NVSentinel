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

package helpers

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// SendHealthEvent sends a health event from a JSON file to the simple-health-client service
func SendHealthEvent(nodeName, eventFilePath string) error {
	eventData, err := os.ReadFile(eventFilePath)
	if err != nil {
		return fmt.Errorf("failed to read health event file %s: %w", eventFilePath, err)
	}

	eventJSON := strings.ReplaceAll(string(eventData), "NODE_NAME", nodeName)

	resp, err := http.Post("http://localhost:8080/health-event", "application/json", strings.NewReader(eventJSON))
	if err != nil {
		return fmt.Errorf("failed to send health event: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("expected status 200, got %d. Response: %s", resp.StatusCode, string(body))
	}

	return nil
}
