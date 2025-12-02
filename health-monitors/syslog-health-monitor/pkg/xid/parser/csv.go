// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package parser

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/patterns"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/metrics"
)

var (
	// reXidNVL5Pattern matches NVIDIA NVL5 XID messages with subcode and intrinfo
	reXidNVL5Pattern = regexp.MustCompile(
		`NVRM: Xid \(PCI:([^)]+)\): (\d+)(?:, pid=[^,]*)?(?:, name=[^,]*)?, ` +
			`(\w+)\s+(\w+)\s+(\w+)\s+(\w+)\s+Link\s+(-?\d+)\s+\((0x[0-9a-fA-F]+)\s+(0x[0-9a-fA-F]+)`,
	)
)

// CSVParser implements Parser interface using local CSV-based error resolution
type CSVParser struct {
	nodeName           string
	errorResolutionMap map[int]types.ErrorResolution
	nvl5Rules          map[int][]common.NVL5DecodingRule
}

// NewCSVParser creates a new CSV parser with error resolution mapping
func NewCSVParser(
	nodeName string,
	errorResolutionMap map[int]types.ErrorResolution,
	nvl5Rules map[int][]common.NVL5DecodingRule,
) *CSVParser {
	return &CSVParser{
		nodeName:           nodeName,
		errorResolutionMap: errorResolutionMap,
		nvl5Rules:          nvl5Rules,
	}
}

// Parse attempts to parse XID information directly from dmesg message
func (p *CSVParser) Parse(message string) (*Response, error) {
	if resp, err := p.parseNVL5XID(message); resp != nil || err != nil {
		if err != nil {
			return nil, fmt.Errorf("failed to parse NVL5 XID from message: %w", err)
		}

		return resp, nil
	}

	resp, err := p.parseStandardXID(message)
	if err != nil {
		return nil, fmt.Errorf("failed to parse standard XID from message: %w", err)
	}

	return resp, nil
}

// parseNVL5XID parses NVL5 XID messages with subcode and intrinfo
func (p *CSVParser) parseNVL5XID(message string) (*Response, error) {
	m := reXidNVL5Pattern.FindStringSubmatch(message)
	if len(m) < 10 {
		return nil, nil
	}

	xidCode, err := strconv.Atoi(m[2])
	if err != nil {
		metrics.XidProcessingErrors.WithLabelValues("xid_parse_error", p.nodeName).Inc()
		return nil, fmt.Errorf("error parsing XID code %s: %w", m[2], err)
	}

	pciAddr := m[1]
	subcode := m[3]
	intrInfo, _ := strconv.ParseInt(m[8], 0, 64)
	errorStatusStr := m[9]

	rules, exists := p.nvl5Rules[xidCode]
	if !exists {
		return nil, nil
	}

	var ruleMnemonic string

	var recommendedAction pb.RecommendedAction

	for _, rule := range rules {
		if p.matchesNVL5Rule(rule, intrInfo, errorStatusStr) {
			slog.Debug("Found matching NVL5 rule", "xidCode", xidCode)

			recommendedAction = common.MapActionStringToProto(rule.Resolution)
			ruleMnemonic = rule.Mnemonic

			break
		}
	}

	decodedXIDStr := fmt.Sprintf("%d.%s", xidCode, subcode)

	xidDetails := XIDDetails{
		Context:       message,
		DecodedXIDStr: decodedXIDStr,
		Mnemonic:      ruleMnemonic,
		Name:          decodedXIDStr,
		Number:        xidCode,
		PCIE:          pciAddr,
		Resolution:    recommendedAction.String(),
	}

	return &Response{
		Success: true,
		Result:  xidDetails,
		Error:   "",
	}, nil
}

// parseStandardXID parses standard XID messages
func (p *CSVParser) parseStandardXID(message string) (*Response, error) {
	// Use the canonical XID pattern from the patterns package directly
	m := patterns.XIDPattern.FindStringSubmatch(message)
	if len(m) < 3 {
		return &Response{Success: false}, nil
	}

	xidCode, err := strconv.Atoi(m[2])
	if err != nil {
		metrics.XidProcessingErrors.WithLabelValues("xid_parse_error", p.nodeName).Inc()
		return nil, fmt.Errorf("error parsing XID code %s: %w", m[2], err)
	}

	pciAddr := m[1]

	recommendedAction := p.getRecommendedActionForXid(xidCode, message)

	xidDetails := XIDDetails{
		DecodedXIDStr: fmt.Sprintf("%d", xidCode),
		Driver:        "",
		Mnemonic:      fmt.Sprintf("XID %d", xidCode),
		Name:          fmt.Sprintf("%d", xidCode),
		Number:        xidCode,
		PCIE:          pciAddr,
		Resolution:    recommendedAction.String(),
	}

	return &Response{
		Success: true,
		Result:  xidDetails,
		Error:   "",
	}, nil
}

func (p *CSVParser) getRecommendedActionForXid(xidCode int, message string) pb.RecommendedAction {
	var recommendedAction = pb.RecommendedAction_CONTACT_SUPPORT
	if errRes, found := p.errorResolutionMap[xidCode]; found {
		recommendedAction = errRes.RecommendedAction
		slog.Info("Found action for XID code",
			"xidCode", xidCode,
			"action", recommendedAction.String())
	} else {
		slog.Info("No action found for XID code, defaulting to CONTACT_SUPPORT",
			"xidCode", xidCode)
	}

	if xidCode == 154 {
		// format is NVRM: Xid (PCI:0008:01:00): 154, GPU recovery action changed from 0x0 (None) to 0x1 (GPU Reset Required)
		lastOpenParan := strings.LastIndex(message, "(")
		lastCloseParan := strings.LastIndex(message, ")")

		if lastOpenParan != -1 && lastCloseParan != -1 {
			// recommendations should be "GPU Reset Required", i.e., the string inside the last ()
			recommendation := message[lastOpenParan+1 : lastCloseParan]

			slog.Debug("recommendation from log", "recommendation", recommendation)

			switch recommendation {
			case "GPU Reset Required", "Drain and Reset":
				recommendedAction = pb.RecommendedAction_COMPONENT_RESET
			case "Node Reboot Required":
				recommendedAction = pb.RecommendedAction_RESTART_BM
			case "None":
				recommendedAction = pb.RecommendedAction_NONE
			default:
				recommendedAction = pb.RecommendedAction_CONTACT_SUPPORT
			}
		} else {
			slog.Warn("xid 154 did not have expected format", "msg", message)
		}
	}

	return recommendedAction
}

func (p *CSVParser) matchesNVL5Rule(rule common.NVL5DecodingRule, intrInfo int64, errorStatusStr string) bool {
	foundMatch := false
	allEmpty := true

	for _, e := range rule.ErrorStatusHex {
		if e == "" {
			continue
		}

		allEmpty = false

		if e == errorStatusStr {
			foundMatch = true
			break
		}
	}

	if !allEmpty && !foundMatch {
		return false
	}

	return p.doesXIDIntrInfoMatchRule(rule.IntrInfoBinary, intrInfo)
}

func (p *CSVParser) doesXIDIntrInfoMatchRule(intrinfoBinaryPattern string, intrInfoInMessage int64) bool {
	messageBinary := fmt.Sprintf("%032b", intrInfoInMessage)

	patternLen := len(intrinfoBinaryPattern)
	messageLen := len(messageBinary)

	if patternLen < messageLen {
		messageBinary = messageBinary[messageLen-patternLen:]
	} else if patternLen > messageLen {
		messageBinary = strings.Repeat("0", patternLen-messageLen) + messageBinary
	}

	for i, patternChar := range intrinfoBinaryPattern {
		if patternChar == '-' {
			continue
		}

		messageChar := rune(messageBinary[i])
		if patternChar != messageChar {
			return false
		}
	}

	return true
}
