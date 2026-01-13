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

package common

import (
	_ "embed"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/thedatashed/xlsxreader"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/patterns"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
)

// XIDPattern is the canonical pattern for detecting XID errors.
// Re-exported from the patterns package for convenience.
var XIDPattern = patterns.XIDPattern

// NVIDIA XID Error Catalog - Embedded Excel File
//
// This embedded Excel file is sourced directly from NVIDIA's official documentation:
// Source Documentation: https://docs.nvidia.com/deploy/xid-errors/analyzing-xid-catalog.html
// Direct Download Link: https://docs.nvidia.com/deploy/xid-errors/_downloads/
// 4586dadb59119a55d1e93a181caa4272/Xid-Catalog.xlsx
//
// The file contains the official NVIDIA XID Error Catalog for Ampere and newer GPUs
// (including PCIe form-factor GPUs). It provides information about:
// - XID error codes and their meanings
// - Recommended immediate actions for system recovery
// - Investigatory actions for root cause analysis
// - GPU applicability (A100, H100, B100, GB200)
// - Error severity and trigger conditions
//
// File Integrity Validation:
// SHA256: 7f70ce9684be0c9d98a367770341ccc1d8bca4465cebe42dab7e23d57df5d5a5
//
// IMPORTANT: This file is used AS-IS from NVIDIA's official source without any
// custom modifications. Any changes to the file content will break the integrity
// validation and may result in incorrect XID error resolution mappings.
//
// To update this file:
// 1. Download the latest version from the official NVIDIA documentation link above
// 2. Replace the existing Xid-Catalog.xlsx file
// 3. Update the SHA256 hash in this comment
// 4. Verify all tests pass with the new file

//go:embed Xid-Catalog.xlsx
var embeddedXidCatalog []byte

// MapActionStringToProto maps action strings to protobuf RecommendedAction values using generated protobuf map
func MapActionStringToProto(s string) pb.RecommendedAction {
	s = strings.ToUpper(strings.TrimSpace(s))

	if value, exists := pb.RecommendedAction_value[s]; exists {
		return pb.RecommendedAction(value)
	}

	switch s {
	// XID_154_EVAL: the guidance is that if an XID 154 is seen along with this error, take that action.
	// If no XID 154 present, RESTART_APP. Since each XID is acted on, the recommendation for XID 154 will be
	// applied as a part of the XID 154 cycle therefore we don't need lookback/lookforward for the XID. Hence
	// XID_154_EVAL is considered equivalent to RESTART_APP.
	case "RESTART_APP", "IGNORE", "XID_154_EVAL":
		return pb.RecommendedAction_NONE
	case "WORKFLOW_XID_48", "RESET_GPU", "RESET_FABRIC":
		return pb.RecommendedAction_COMPONENT_RESET
	default:
		slog.Warn("Unknown action string, defaulting to CONTACT_SUPPORT", "action", s)
		return pb.RecommendedAction_CONTACT_SUPPORT
	}
}

// NVL5DecodingRule represents a rule for NVL5 XID decoding (144-150) from the Excel sheet
type NVL5DecodingRule struct {
	XIDNumber      int
	IntrInfoBinary string   // V1 binary pattern for matching
	ErrorStatusHex []string // List of error status values as strings
	Resolution     string
	Severity       string
	Mnemonic       string
}

func LoadErrorResolutionMap() (map[int]types.ErrorResolution, error) {
	errorResolutionMap := make(map[int]types.ErrorResolution)

	slog.Info("Loading XID error resolution map from embedded Excel file")

	xl, err := xlsxreader.NewReader(embeddedXidCatalog)
	if err != nil {
		return errorResolutionMap, fmt.Errorf("failed to create Excel reader from embedded data: %w", err)
	}

	targetSheet, err := findXidsSheet(xl)
	if err != nil {
		return errorResolutionMap, fmt.Errorf("failed to find Xids sheet: %w", err)
	}

	errorResolutionMap, err = processXidsSheet(xl, targetSheet, errorResolutionMap)
	if err != nil {
		return errorResolutionMap, fmt.Errorf("failed to process Xids sheet: %w", err)
	}

	return errorResolutionMap, nil
}

// loadNVL5DecodingRules loads NVL5-specific decoding rules using exact column indices
func loadNVL5DecodingRules(xl *xlsxreader.XlsxFile) (map[int][]NVL5DecodingRule, error) {
	nvl5DecodingMap := make(map[int][]NVL5DecodingRule)

	sheetName := "Xid 144-150 Decode"

	if !slices.Contains(xl.Sheets, sheetName) {
		return nvl5DecodingMap, fmt.Errorf("sheet '%s' not found", sheetName)
	}

	slog.Info("Loading NVL5 XID decoding rules", "sheet", sheetName)

	rowIndex := 0

	for row := range xl.ReadRows(sheetName) {
		if rowIndex == 0 {
			// Skip header row
			rowIndex++
			continue
		}

		rule, err := parseNVL5Row(row)
		if err != nil {
			slog.Debug("Skipping NVL5 row", "row", rowIndex+1, "error", err)
			continue
		}

		if rule != nil {
			nvl5DecodingMap[rule.XIDNumber] = append(nvl5DecodingMap[rule.XIDNumber], *rule)
		}

		rowIndex++
	}

	slog.Info("Successfully loaded NVL5 decoding rules", "xidTypeCount", len(nvl5DecodingMap))

	return nvl5DecodingMap, nil
}

// parseNVL5Row parses a single row from the NVL5 sheet using column letters to handle gaps
func parseNVL5Row(row xlsxreader.Row) (*NVL5DecodingRule, error) {
	// Column A: 'Xid'
	// Column B: 'Subcode V1(<R575)/V2(>=R575)...' - This is actually intrInfo decode info, not subcode
	// Column C: '(V1(<R575)) IntrInfo decode...' - This is the binary pattern for matching
	// Column E: 'Error Status (hex)'
	// Column F: 'Resolution Bucket (Data Center Recovery Action)'
	// Column K: 'Severity...' - Column K, but may have gaps
	columnValues := make(map[string]string)
	for _, cell := range row.Cells {
		columnValues[cell.Column] = cell.Value
	}

	xidStr := strings.TrimSpace(columnValues["A"])
	if xidStr == "" {
		return nil, fmt.Errorf("empty XID value")
	}

	xidCode, err := strconv.Atoi(xidStr)
	if err != nil {
		return nil, fmt.Errorf("invalid XID code: %s", xidStr)
	}

	rule := &NVL5DecodingRule{
		XIDNumber: xidCode,
	}

	rule.IntrInfoBinary = strings.TrimSpace(columnValues["C"])

	errorStatusStr := columnValues["E"]
	for errStatus := range strings.SplitSeq(errorStatusStr, "/") {
		rule.ErrorStatusHex = append(rule.ErrorStatusHex, errStatus)
	}

	rule.Resolution = strings.TrimSpace(columnValues["F"])
	rule.Severity = strings.TrimSpace(columnValues["K"])
	rule.Mnemonic = strings.TrimSpace(columnValues["B"])

	return rule, nil
}

// GetNVL5DecodingRules returns the NVL5 decoding rules for use in parsing
func GetNVL5DecodingRules() (map[int][]NVL5DecodingRule, error) {
	xl, err := xlsxreader.NewReader(embeddedXidCatalog)
	if err != nil {
		return nil, fmt.Errorf("failed to create Excel reader from embedded data: %w", err)
	}

	return loadNVL5DecodingRules(xl)
}

func findXidsSheet(xl *xlsxreader.XlsxFile) (string, error) {
	for _, sheet := range xl.Sheets {
		if sheet == "Xids" {
			return sheet, nil
		}
	}

	return "", fmt.Errorf("'Xids' sheet not found in embedded Excel file")
}

func processXidsSheet(xl *xlsxreader.XlsxFile, targetSheet string,
	errorResolutionMap map[int]types.ErrorResolution) (map[int]types.ErrorResolution, error) {
	rowIndex := 0
	for row := range xl.ReadRows(targetSheet) {
		if rowIndex == 0 {
			if err := validateHeader(row); err != nil {
				return errorResolutionMap, fmt.Errorf("header validation failed: %w", err)
			}

			rowIndex++

			continue
		}

		if err := processDataRow(row, rowIndex, errorResolutionMap); err != nil {
			return errorResolutionMap, fmt.Errorf("failed to process data row %d: %w", rowIndex+1, err)
		}

		rowIndex++
	}

	if len(errorResolutionMap) == 0 {
		return errorResolutionMap, fmt.Errorf("no valid XID mappings found in embedded Excel file")
	}

	slog.Info("Successfully loaded XID error resolution mappings", "count", len(errorResolutionMap))

	return errorResolutionMap, nil
}

func validateHeader(row xlsxreader.Row) error {
	if len(row.Cells) < 9 {
		return fmt.Errorf(
			"invalid Excel format: header row has %d columns, expected at least 9 "+
				"(Type, Code, Mnemonic, Description, Applies to A100/H100/B100/GB200, Resolution Bucket)",
			len(row.Cells))
	}

	return nil
}

func processDataRow(row xlsxreader.Row, rowIndex int, errorResolutionMap map[int]types.ErrorResolution) error {
	if len(row.Cells) < 9 {
		slog.Warn("Row has insufficient columns, skipping",
			"row", rowIndex+1,
			"columns", len(row.Cells),
			"expected", 9)

		return nil
	}

	// 0: Type (XID), 1: Code, 2: Mnemonic, 3: Description, 4-7: Applies to GPUs, 8: Resolution Bucket (Immediate Action)
	codeStr := strings.TrimSpace(row.Cells[1].Value)

	actionStr := strings.TrimSpace(row.Cells[8].Value)

	if codeStr == "" {
		return fmt.Errorf("row %d: empty XID code - Excel sheet format is invalid", rowIndex+1)
	}

	if actionStr == "" {
		slog.Warn("No action found for XID code, skipping",
			"row", rowIndex+1,
			"xidCode", codeStr)

		return nil
	}

	code, err := strconv.Atoi(codeStr)
	if err != nil {
		return fmt.Errorf(
			"row %d: error parsing XID code %s: %w - Excel sheet format is invalid",
			rowIndex+1, codeStr, err)
	}

	action := MapActionStringToProto(actionStr)

	errRes := types.ErrorResolution{
		RecommendedAction: action,
	}
	errorResolutionMap[code] = errRes

	slog.Debug("Loaded XID mapping", "xidCode", code, "action", actionStr)

	return nil
}
