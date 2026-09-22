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

package common

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/require"
	"github.com/thedatashed/xlsxreader"
)

// The resolution buckets in Xid-Catalog.xlsx and the cases in MapActionStringToProto
// are two halves of one contract, and nothing currently holds them together. The
// workbook can be re-exported and the code not updated, or a case can be written for a
// bucket string that the sheet never contained. Both failure modes are silent: an
// unmatched bucket falls through to CONTACT_SUPPORT, which looks like a severe fault
// rather than like a missing mapping.
//
// These two tests close it in both directions. The allowlists are the point as much as
// the assertions: they record in the source which buckets are deliberately human-triage
// and which cases are deliberately staged ahead of the catalogue, so that "no handler"
// and "intended for a human" stop being indistinguishable from the outside. See
// https://github.com/NVIDIA/NVSentinel/issues/1888.

// manualTriageBuckets are catalogue buckets that intentionally resolve to
// CONTACT_SUPPORT. A human decides; there is no automated recovery to select. Confirmed
// intentional by the catalogue owners on issue #1888.
var manualTriageBuckets = []string{
	"CHECK_UVM",
	"UPDATE_SWFW",
	"CHECK_MECHANICALS",
	"CONTACT_SUPPORT",
}

// handledElsewhere are catalogue buckets that legitimately do not resolve in this
// mapper because a different code path acts on the XID before the bucket is consulted.
// Their falling through here is correct, not a gap.
var handledElsewhere = map[string]string{
	"XID_154":              "xidCode == 154 override in pkg/xid/parser/csv.go parses the action out of the log line",
	"WORKFLOW_NVLINK_ERR":  "XID 74: xidCode == 74 override plus the XID74Reg* health-events-analyzer rules",
	"WORKFLOW_NVLINK5_ERR": "XIDs 144-150: resolved from column F of the 'Xid 144-150 Decode' sheet by parseNVL5Row",
}

// cellByColumn returns the trimmed value of a cell by its spreadsheet column letter.
//
// Selecting by coordinate rather than by slice index is required for correctness, not
// style. xlsxreader omits empty cells and does not pad Row.Cells, so the Nth element is
// the Nth *populated* cell, not column N. In this workbook the XID 163 to 170 rows have
// an empty column I and a populated column L, so row.Cells[8] on those rows yields the
// column L note rather than the resolution bucket.
func cellByColumn(row xlsxreader.Row, column string) string {
	for _, cell := range row.Cells {
		if cell.Column == column {
			return strings.TrimSpace(cell.Value)
		}
	}

	return ""
}

// stagedForFutureCatalogue are non-default cases in MapActionStringToProto that no
// current catalogue cell selects. They are kept deliberately so that a future workbook
// revision populating the corresponding rows starts working without a code change.
//
// Each entry names the XID it is waiting on. When a revision does populate that row,
// TestEveryMapperCaseIsReachable stops reporting the entry as unreachable and the entry
// should be deleted from this list.
var stagedForFutureCatalogue = map[string]string{
	"RECOVER_FEATURE_RESET_GPU": "XID 163: bucket cell Xids!I164 is empty. The column L note on that " +
		"row (\"reload the driver or reset the GPU\") is what this case was written against",
	"WORKFLOW_XID_168": "XID 168: bucket cell Xids!I169 is empty. The column L note on that " +
		"row (\"boot re-attempted with shifted WPR\") is what this case was written against",
	"RESET_FABRIC": "no catalogue cell currently selects this bucket, in any sheet",
}

// catalogueBucket is a resolution-bucket string as it appears in the workbook, with the
// sheet and cell it came from so a failure names something an operator can open.
type catalogueBucket struct {
	value string
	where string
}

// readCatalogueBuckets returns every non-empty resolution bucket in the embedded
// workbook: column I of the Xids sheet, and column F of the NVLink-5 decode sheet.
//
// Empty cells are deliberately excluded. processDataRow skips a row with an empty
// bucket, so such an XID never enters the resolution map at all and is resolved by the
// caller's default rather than by this mapper. That is a different mechanism from an
// unmatched bucket string and is not what these tests are about.
func readCatalogueBuckets(t *testing.T) []catalogueBucket {
	t.Helper()

	xl, err := xlsxreader.NewReader(embeddedXidCatalog)
	require.NoError(t, err, "embedded Xid-Catalog.xlsx must be readable")

	var buckets []catalogueBucket

	require.Contains(t, xl.Sheets, "Xids", "workbook must contain the Xids sheet")

	rowIndex := 0
	for row := range xl.ReadRows("Xids") {
		require.NoErrorf(t, row.Error, "failed to read row %d of the Xids sheet", rowIndex+1)

		rowIndex++
		if rowIndex == 1 {
			continue
		}

		if v := cellByColumn(row, "I"); v != "" {
			buckets = append(buckets, catalogueBucket{
				value: v,
				where: "Xids!I" + strconv.Itoa(rowIndex),
			})
		}
	}

	const decodeSheet = "Xid 144-150 Decode"

	require.Contains(t, xl.Sheets, decodeSheet, "workbook must contain the NVLink-5 decode sheet")

	rowIndex = 0
	for row := range xl.ReadRows(decodeSheet) {
		require.NoErrorf(t, row.Error, "failed to read row %d of the %s sheet", rowIndex+1, decodeSheet)

		rowIndex++
		if rowIndex == 1 {
			continue
		}

		if v := cellByColumn(row, "F"); v != "" {
			buckets = append(buckets, catalogueBucket{
				value: v,
				where: decodeSheet + "!F" + strconv.Itoa(rowIndex),
			})
		}
	}

	require.NotEmpty(t, buckets, "read no buckets at all: the workbook layout has changed")

	return buckets
}

// TestEveryCatalogueBucketResolves asserts that every bucket string the workbook
// actually contains reaches a real mapping, or is an acknowledged manual-triage bucket.
//
// This is the direction that bit us: a bucket the mapper does not know falls through to
// CONTACT_SUPPORT, which is indistinguishable from a genuine vendor escalation.
func TestEveryCatalogueBucketResolves(t *testing.T) {
	unresolved := map[string][]string{}

	for _, b := range readCatalogueBuckets(t) {
		normalised := strings.ToUpper(strings.TrimSpace(b.value))

		if MapActionStringToProto(b.value) != pb.RecommendedAction_CONTACT_SUPPORT {
			continue
		}

		switch {
		case slices.Contains(manualTriageBuckets, normalised):
			// Intentionally a human decision.
		case handledElsewhere[normalised] != "":
			// Another code path acts on this XID before the bucket is consulted.
		default:
			unresolved[normalised] = append(unresolved[normalised], b.where)
		}
	}

	if len(unresolved) == 0 {
		return
	}

	keys := make([]string, 0, len(unresolved))
	for k := range unresolved {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var b strings.Builder

	b.WriteString("catalogue buckets fall through to CONTACT_SUPPORT with no mapping:\n")

	for _, k := range keys {
		cells := unresolved[k]
		if len(cells) > 4 {
			cells = append(cells[:4:4], "...")
		}

		b.WriteString("  " + k + "  (" + strings.Join(cells, ", ") + ")\n")
	}

	b.WriteString("\nAdd a case to MapActionStringToProto, or if the bucket is intended for " +
		"human triage add it to manualTriageBuckets in this file.")

	t.Fatal(b.String())
}

// mapperCaseStrings extracts the string literals from the case clauses of the type
// switch in MapActionStringToProto, by parsing common.go.
//
// Reading the source rather than restating the list is deliberate: a hardcoded copy
// would itself drift, and the case a reviewer is most likely to add without thinking
// about the catalogue is exactly the one a hardcoded list would miss.
func mapperCaseStrings(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "common.go", nil, 0)
	require.NoError(t, err, "common.go must be parseable from the package directory")

	var found []string

	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "MapActionStringToProto" {
			return true
		}

		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			clause, ok := inner.(*ast.CaseClause)
			if !ok {
				return true
			}

			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}

				if s, err := strconv.Unquote(lit.Value); err == nil && s != "" {
					found = append(found, s)
				}
			}

			return true
		})

		return false
	})

	require.NotEmpty(t, found,
		"parsed no case strings from MapActionStringToProto: the function shape has changed "+
			"and this test is no longer checking anything")

	return found
}

// TestEveryMapperCaseIsReachable asserts the converse: every non-default case in
// MapActionStringToProto is selected by some catalogue cell, or is explicitly staged for
// a future revision.
//
// This catches a case whose spelling drifts away from the sheet, which would otherwise
// be invisible because the bucket simply falls through to the default. It is also the
// check that tells you when a workbook revision has made a staged case live, which is
// the moment the allowlist entry should be deleted.
func TestEveryMapperCaseIsReachable(t *testing.T) {
	inCatalogue := map[string]bool{}
	for _, b := range readCatalogueBuckets(t) {
		inCatalogue[strings.ToUpper(strings.TrimSpace(b.value))] = true
	}

	var unreachable []string

	for _, c := range mapperCaseStrings(t) {
		normalised := strings.ToUpper(strings.TrimSpace(c))
		if inCatalogue[normalised] {
			continue
		}

		if reason, staged := stagedForFutureCatalogue[normalised]; staged {
			t.Logf("case %q is staged and still unreachable: %s", normalised, reason)
			continue
		}

		unreachable = append(unreachable, normalised)
	}

	sort.Strings(unreachable)

	require.Emptyf(t, unreachable,
		"these MapActionStringToProto cases match no bucket in the embedded catalogue: %v\n"+
			"Either the spelling has drifted from the workbook, or the case is intended for a "+
			"future catalogue revision and belongs in stagedForFutureCatalogue with the XID it "+
			"is waiting on.", unreachable)
}

// TestStagedCasesAreStillStaged fails once a workbook revision populates a cell for a
// case currently listed in stagedForFutureCatalogue, so the stale allowlist entry gets
// removed rather than quietly outliving its reason.
func TestStagedCasesAreStillStaged(t *testing.T) {
	inCatalogue := map[string]bool{}
	for _, b := range readCatalogueBuckets(t) {
		inCatalogue[strings.ToUpper(strings.TrimSpace(b.value))] = true
	}

	var nowLive []string

	for staged := range stagedForFutureCatalogue {
		if inCatalogue[strings.ToUpper(strings.TrimSpace(staged))] {
			nowLive = append(nowLive, staged)
		}
	}

	sort.Strings(nowLive)

	require.Emptyf(t, nowLive,
		"these cases are listed in stagedForFutureCatalogue but the catalogue now selects them: %v\n"+
			"The wait is over: delete each one from stagedForFutureCatalogue.", nowLive)
}
