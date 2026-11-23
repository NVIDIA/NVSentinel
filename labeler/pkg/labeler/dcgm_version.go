// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package labeler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

var (
	// dcgmImageVersionRegex matches the version in a DCGM image tag. The minor and patch versions are optional.
	dcgmImageVersionRegex = regexp.MustCompile(`dcgm:(\d+)\.(?:(\d+)\.(\d+))?`)
	laneMinVersionRegex   = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
)

// DCGMVersionLane maps the DCGM versions from MinVersion up to the next lane to a dcgm.version label value.
type DCGMVersionLane struct {
	MinVersion string `json:"minVersion"`
	Label      string `json:"label"`
}

// DCGMVersionLanes are validated lanes, sorted by minimum version.
type DCGMVersionLanes struct {
	lanes []dcgmVersionLane
}

type dcgmVersionLane struct {
	minVersion [3]int
	label      string
}

// ParseDCGMVersionLanes parses and validates a JSON list of DCGMVersionLane. The order of the list does not matter.
func ParseDCGMVersionLanes(data string) (DCGMVersionLanes, error) {
	var input []DCGMVersionLane

	if strings.TrimSpace(data) == "" {
		return DCGMVersionLanes{}, errors.New("DCGM version lanes are required")
	}

	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&input); err != nil {
		return DCGMVersionLanes{}, fmt.Errorf("parse DCGM version lanes: %w", err)
	}

	if len(input) == 0 {
		return DCGMVersionLanes{}, errors.New("DCGM version lanes: at least one lane is required")
	}

	lanes := make([]dcgmVersionLane, 0, len(input))

	for _, lane := range input {
		m := laneMinVersionRegex.FindStringSubmatch(lane.MinVersion)
		if m == nil {
			return DCGMVersionLanes{}, fmt.Errorf("DCGM version lanes: minVersion %q must be X.Y.Z", lane.MinVersion)
		}

		minVersion, err := parseDCGMVersion(m[1], m[2], m[3])
		if err != nil {
			return DCGMVersionLanes{}, fmt.Errorf("DCGM version lanes: minVersion %q: %w", lane.MinVersion, err)
		}

		if lane.Label == "" || len(validation.IsValidLabelValue(lane.Label)) > 0 {
			return DCGMVersionLanes{}, fmt.Errorf("DCGM version lanes: label %q must be a non-empty label value",
				lane.Label)
		}

		if slices.ContainsFunc(lanes, func(l dcgmVersionLane) bool { return l.minVersion == minVersion }) {
			return DCGMVersionLanes{}, fmt.Errorf("DCGM version lanes: duplicate minVersion %q", lane.MinVersion)
		}

		lanes = append(lanes, dcgmVersionLane{minVersion: minVersion, label: lane.Label})
	}

	slices.SortFunc(lanes, func(a, b dcgmVersionLane) int {
		return slices.Compare(a.minVersion[:], b.minVersion[:])
	})

	return DCGMVersionLanes{lanes: lanes}, nil
}

// labelForImage returns the label of the lane with the highest minimum version that is not above the DCGM version
// of the image. It returns "" if the image is not a DCGM image or its version is below the lowest lane. A tag without
// minor and patch versions maps to <major>.0.0.
func (l DCGMVersionLanes) labelForImage(image string) string {
	m := dcgmImageVersionRegex.FindStringSubmatch(image)
	if m == nil {
		return ""
	}

	minor, patch := m[2], m[3]
	if minor == "" {
		slog.Warn("Cannot parse DCGM minor and patch version from image; using major version only", "image", image)

		minor, patch = "0", "0"
	}

	version, err := parseDCGMVersion(m[1], minor, patch)
	if err != nil {
		slog.Warn("Cannot parse DCGM version from image", "image", image, "error", err)
		return ""
	}

	label := ""

	for _, lane := range l.lanes {
		if slices.Compare(lane.minVersion[:], version[:]) > 0 {
			break
		}

		label = lane.label
	}

	return label
}

// isLabel reports whether value is the label of a lane.
func (l DCGMVersionLanes) isLabel(value string) bool {
	value = strings.TrimSpace(value)

	return slices.ContainsFunc(l.lanes, func(lane dcgmVersionLane) bool { return lane.label == value })
}

func parseDCGMVersion(major, minor, patch string) ([3]int, error) {
	var version [3]int

	for i, part := range []string{major, minor, patch} {
		n, err := strconv.Atoi(part)
		if err != nil {
			return version, err
		}

		version[i] = n
	}

	return version, nil
}
