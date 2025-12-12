# Implement WORKFLOW_NVLINK_ERR

## Overview

WORKFLOW_NVLINK_ERR is a resolution workflow for XID 74 error [(NVLink_ERROR)](https://docs.nvidia.com/deploy/xid-errors/analyzing-xid-catalog.html). This workflow provides detailed bit-level analysis of NVLink error registers to determine the root cause and appropriate remediation action.

The explanation of WORKFLOW_NVLINK_ERR in RAS Catalog doc is defined like this:

```text
 Extract the hex strings from the Xid error message. 
 Note that there should be seven fields in the Xid. Unused fields would expect to be 0x0 rather than a full DWORD of 0’s. 
 The first, third, fourth and fifth registers are valid for bits analysis. 
 Evaluate the populate(d) registers. If bits other than those specifically outlined below are seen, please report a bug. 
 First register:
 Bit 0, 23, 30: Can be safely ignored.
 Bits 1, 20: These are generally sympathetic or secondary errors. If seen with other bits set or other Xid/SXid, please follow the resolution for those. If seen solo, please report a bug. 
 Bits 4 or 5: Likely HW issue with ECC/Parity --> If seen more than 2 times on the same link, report a bug.
 Bits 8, 9, 12, 16, 17, 24, 28: Could possibly be a HW issue: Check link mechanical connections and re-seat if a field resolution is required. Run diags if issue persists. If the issue persist, and diagnostics has passed please report a bug. 
 Bits 21 or 22: Marginal channel SI issue. If other errors accompany this Xid, follow the resolution for those first. Otherwise, check link mechanical connections. Run Field Diags and report a bug. 
 Bits 27, 29: If seen repeatedly, please report a bug. 
 Third register:
 Bits 0, 1, 2, 6: Likely HW issue with ECC/Parity --> If seen more than 2 times on the same link, report a bug.
 Bit 13: Not expected to be seen in production. If seen, please report a bug. 
 Bits 16, 19: If seen repeatedly, please run Field Diags and report a bug
 Bits 17, 18: If seen repeatedly, please report a bug.
 Fourth register:
 Bits 16, 17: These are generally sympathetic or secondary errors. If seen with other bits set or other Xid/SXid, please follow the resolution for those. If seen solo, please report a bug. 
 Bit 18: These are generally sympathetic or secondary errors, though a reset of the fabric is required. If seen with other bits set or other Xid/SXid, please follow the resolution for those. If seen solo, please report a bug.
 Fifth register:
 Bits 18, 19, 21, 22, 24, 25, 27, 28: Likely HW issue with ECC/Parity --> If seen more than 2 times on the same link, report a bug.
 Bits 20, 23, 26, 29: These errors represent a threshold of ECC errors being exceeded. There was no uncorrectable error at this time. Continue operation. If desired, Field Diags can be run to check for link integrity.
 ```

## XID 74 Description

XID 74 is logged when the GPU detects a problem with a connection from the GPU to another GPU or NVSwitch over NVLink.

**Key Characteristics:**
- Indicates hardware failure with the NVLink connection itself
- If one GPU fail, it might be possible that connected GPU also trigger XID 74 because of connected NVLink is down
- May indicate a problem with the remote device at the other end of the link
- Use `nvidia-smi nvlink` command for additional NVLink error details and connection information

### Message Format
```
[449410.332316] NVRM: Xid (PCI:0003:00:00): 74, pid='<unknown>', name=<unknown>, NVLink: fatal error detected on link 14(0x0, 0x0, 0x10000, 0x0, 0x0, 0x0, 0x0)
```
The error message contains **7 hex register values** in parentheses:

## Decision Flow Diagram

The following diagram shows how health-events-analyzer processes XID 74 events to find best remediation action by analyzing the bits and registers.

```
   ┌─────────────────────────────────┐
   │     XID 74 Event Received       │
   └────────────┬────────────────────┘
                │
       ┌──────────────────┐
       │ Analyze Register │
       │ Bit Patterns     │
       └────────┬─────────┘
                │
    ┌───────────┼───────────┐
    │           │           │
    ▼           ▼           ▼
┌─────────┐ ┌─────────┐ ┌─────────┐
│Register │ │Register │ │Register │
│   0     │ │   2     │ │  3 & 4  │
│Analysis │ │Analysis │ │Analysis │
└────┬────┘ └────┬────┘ └────┬────┘
     │           │           │
     └───────────┼───────────┘
                 │
                 ▼
        ┌────────────────┐
        │ For "repeated" │
        │ conditions:    │
        │check repetition│
        │   on same      |
        |  GPU+NVLink    │
        └────────┬───────┘
                 │
                 ▼
        ┌────────────────┐
        │ Apply Action   │
        │ (CONTACT_      │
        │  SUPPORT,      │
        │  COMPONENT_    │
        │  RESET, NONE)  │
        └────────────────┘
```

### Detailed Register Analysis Flow

#### Register 0 (First Register) Decision Tree:

```
Register 0
    │
    ├─ Bits 1, 20 set? 
    |   These are sympathetic errors (or secondary errors) that occur as a consequence or side effect of other primary errors, rather than being the root cause
    |   themselves. If accompanied by other errors, follow their resolution first.
    │   └─ Solo (only XID 74 occurred and no other bits are set)?
    |        recommendedAction = CONTACT_SUPPORT
    |        message = "unexpected error, please open NVBug"
    |        isFatal = true
    │
    ├─ Bits 4 or 5 set?
    |   Likely a HW issue with ECC/Parity.
    │   └─ Seen >2x on same link and GPU?
    |        recommendedAction = CONTACT_SUPPORT
    |        message = "likely a HW issue with ECC/Parity"
    |        isFatal = true
    │   
    ├─ Bits 8, 9, 12, 16, 17, 24, 28 set?
    |   Could possibly be a HW issue.
    |        recommendedAction = CONTACT_SUPPORT
    |        message = "could be a hardware issue, request to check link mechanical connections and run field diagnosis if issue persists"
    |        isFatal = true
    │
    ├─ Bits 21 or 22 set?
    |   Marginal channel SI (signal integrity) issue. If accompanied by other errors, follow their resolution first.
    │   └─ Solo (only XID 74 occurred and no other bits are set)?
    |        recommendedAction = CONTACT_SUPPORT
    |        message = "marginal SI issue, request to check link mechanical connections and run field diagnosis if issue persists"
    |        isFatal = true
    │
    └─ Bits 27, 29 set?
         └─ Seen repeated XID 74 with same bit (27, 29) set (2x or more on same GPU)?
              recommendedAction = CONTACT_SUPPORT
              message = "unexpected error, please open NVBug"
              isFatal = true

```

#### Register 2 (Third Register) Decision Tree:

```
Register 2
    │
    ├─ Bits 0, 1, 2, 6 set?
    |   Likely a HW issue with ECC/Parity.
    │   └─ Seen >2x on same link and GPU?
    |        recommendedAction = CONTACT_SUPPORT
    |        message = "likely a HW issue with ECC/Parity, repeating on same NVLink"
    |        isFatal = true
    │
    ├─ Bit 13 set?
    |        recommendedAction = CONTACT_SUPPORT
    |        message = "unexpected error, please open NVBug"
    |        isFatal = true
    │
    ├─ Bits 16, 19 set?
    │   └─ Seen repeated XID 74 with same bit (16, 19) set (2x or more on same GPU)?
    |        recommendedAction = CONTACT_SUPPORT
    |        message = "request for field diagnosis"
    |        isFatal = true
    │
    └─ Bits 17, 18 set?
         └─ Seen repeated XID 74 with same bit (17, 18) set (2x or more on same GPU)?
              recommendedAction = CONTACT_SUPPORT
              message = "unexpected error, please open NVBug"
              isFatal = true
```

#### Register 3 (Fourth Register) Decision Tree:
,
```
Register 3
    │
    ├─ Bits 16, 17 set?
    |   These are sympathetic errors (or secondary errors) that occur as a consequence or side effect of other primary errors, rather than being the root cause
    |   themselves. If accompanied by other errors, follow their resolution first.
    │   └─ If seen solo (only XID 74 occurred and no other bits are set)?
    |        recommendedAction = CONTACT_SUPPORT
    |        message = "unexpected error, please open NVBug"
    |        isFatal = true
    │
    └─ Bit 18 set?
         These are sympathetic errors (or secondary errors) that occur as a consequence or side effect of other primary errors, rather than being the root cause themselves. If accompanied by other errors, follow their resolution first.
         └─ If seen solo (only XID 74 occurred and no other bits are set)?
              recommendedAction = COMPONENT_RESET
              message = "reset of fabric is required"
              isFatal = true
```

#### Register 4 (Fifth Register) Decision Tree:

```
Register 4
    │
    ├─ Bits 18, 19, 21, 22, 24, 25, 27, 28 set?
    |   Likely a HW issue with ECC/Parity.
    │   └─ Seen >2x on same link and GPU?
    |        recommendedAction = CONTACT_SUPPORT
    |        message = "likely a HW issue with ECC/Parity, repeating on same NVLink"
    |        isFatal = true
    │
    └─ Bits 20, 23, 26, 29 set?
         These errors represent a threshold of ECC errors being exceeded. There was no uncorrectable error at this time. Continue operation.
         If desired, Field Diags can be run to check for link integrity.
              recommendedAction = NONE
              message = "request for field diagnosis if user jobs are interrupted or error occurs repeatedly"
              isFatal = false
```
---

## Syslog-Health-Monitor Code Changes

### Architecture Detection Support

#### 1. Add XID 74 NVLink Error Pattern 
Extract NVLink ID and 7 register values

**File:** `health-monitors/syslog-health-monitor/pkg/xid/parser/csv.go`

Add new regex pattern for XID 74 with NVLink registers:

```go
var (
    // reXid74NVLinkPattern matches XID 74 NVLink errors with 7 registers
    // Example: "NVLink: fatal error detected on link 14(0x0, 0x0, 0x10000, 0x0, 0x0, 0x0, 0x0)"
    reXid74NVLinkPattern = regexp.MustCompile(
        `link\s+(\d+)\(` +
        `(0x[0-9a-fA-F]+),\s*` + // Register 0
        `(0x[0-9a-fA-F]+),\s*` + // Register 1
        `(0x[0-9a-fA-F]+),\s*` + // Register 2
        `(0x[0-9a-fA-F]+),\s*` + // Register 3
        `(0x[0-9a-fA-F]+),\s*` + // Register 4
        `(0x[0-9a-fA-F]+),\s*` + // Register 5
        `(0x[0-9a-fA-F]+)\)`,    // Register 6
    )
    reXid13Pattern = regexp.MustCompile(`\(GPC\s+(\d+),\s*TPC\s+(\d+),\s*SM\s+(\d+)\)`)
)
```

#### 2. Add XID 74 Metadata Extraction in parseStandardXID

**File:** `health-monitors/syslog-health-monitor/pkg/xid/parser/csv.go`

In the `parseStandardXID` method, add XID 74 handling:

```go
func (p *CSVParser) parseStandardXID(message string) (*Response, error) {
    // ... existing code for parsing XID number and PCI address and XID 13 ...
    
    // NEW: XID 74 NVLink handling
    if xidCode == 74 {
        nvLinkMetadata := fetchXID74NVLinkData(message)
        
        // Merge NVLink metadata into main metadata map
        for k, v := range nvLinkMetadata {
            metadata[k] = v
        }
    }
    
    xidDetails := XIDDetails{
        // ... existing fields ...
        Metadata: metadata,
    }
    
    return &Response{Success: true, Result: xidDetails}, nil
}
```

#### 3. Add Parsing Helper Functions

1. **fetchXID74NVLinkData:** To fetch the NVLink ID and all registers data.

2. **convertHexToBinary32:** Convert hex register values to 32-bit binary strings to enable bit-level analysis. Binary representation allows health-events-analyzer to easily check if specific bit positions (e.g., bit 4, bit 16) are set to 1 or 0, which is required for the WORKFLOW_NVLINK_ERR decision logic.

**File:** `health-monitors/syslog-health-monitor/pkg/xid/parser/csv.go`

```go
// fetchXID74NVLinkData extracts link number and 7 register values from XID 74 NVLink messages
// Returns map with NVLINK and XID74_REG0_BIN through XID74_REG6_BIN keys
func fetchXID74NVLinkData(message string) (map[string]string) {
    matches := reXid74NVLinkPattern.FindStringSubmatch(message)

    if len(matches) == 0 {
      return nil // Not an XID 74 NVLink error
    }

    metadata := make(map[string]string)
    nvlink := matches[1]

    metadata["NVLINK"] = nvlink

    for i := 2; i < len(matches); i++ {
      metadata[fmt.Sprintf("REG%d", i-2)] = convertHexToBinary32(matches[i])
    }

    return metadata
}

func convertHexToBinary32(hexStr string) string {
    value, err := strconv.ParseInt(hexStr, 0, 64)
    if err != nil {
        slog.Warn("Failed to parse hex value", "hex", hexStr, "error", err)
        return strings.Repeat("0", 32)
    }
    
    // Format as 32-bit binary string (pad with leading zeros)
    return fmt.Sprintf("%032b", value)
}
```
**NOTE** Similar changes will be required in XID-analyzer script to parse NVLink ID and registers data. 

#### 4. Health Event Structure

The XID parser returns metadata that gets stored in the health event's `EntitiesImpacted` field:

```javascript
{
  _id: ObjectId("..."),
  healthevent: {
    errorcode: ["74"],
    checkName: "SysLogsXIDError",
    entitiesimpacted: [
      {entitytype: "PCI", entityvalue: "0009:01:00"},
      {entitytype: "GPU_UUID", entityvalue: "GPU-abc123"},
      {entitytype: "NVLINK", entityvalue: "14"},
      {entitytype: "REG0", entityvalue: "00000000000000000000000000000000"},
      {entitytype: "REG1", entityvalue: "00000000000000000000000000000000"},
      {entitytype: "REG2", entityvalue: "00000000000000010000000000000000"},
      {entitytype: "REG3", entityvalue: "00000000000000000000000000000000"},
      {entitytype: "REG4", entityvalue: "00000000000000000000000000000000"},
      {entitytype: "REG5", entityvalue: "00000000000000000000000000000000"},
      {entitytype: "REG6", entityvalue: "00000000000000010000000000000000"},
    ]
    generatedtimestamp: {seconds: 1699900000},
    recommendedAction: "RESTART_APP"
  }
}
```

**Key Fields:**
- `entitiesimpacted[].NVLINK`: The NVLink ID to find the repetition on same NVLink
- `entitiesimpacted[].REG0-REG6`: Binary representation of the 7 register values for bit-level analysis


## Health-Events-Analyzer Code Changes

We need to define new rules for WORKFLOW_NVLINK_ERR, where each rule performs necessary three-step check:

1. **XID Verification**: As this WORKFLOW_NVLINK_ERR is only recommended for XID 74 so, we need to run these rules only for XID 74. We need to check if `errorcode`in received event is "74" or not. If yes, then evaluate the rules by checking bits and registers.  
2. **Bit Pattern Analysis**: Check specific bits in registers (REG0, REG2, REG3, REG4) from `entitiesimpacted[]` (as we have made changes in syslog monitor to store this info) and apply repetition checks (query last 24h) to determine repetition on same GPU+NVLink combination.

Each rule maps to one specific scenario from the decision trees above (e.g., "REG0 bits 4/5 repeated >2x → CONTACT_SUPPORT", "REG4 bits 20/23/26/29 → NONE")

---

## References

- [NVIDIA XID Error Catalog](https://docs.nvidia.com/deploy/xid-errors/analyzing-xid-catalog.html)
- [GPU Debug Guidelines](https://docs.nvidia.com/deploy/gpu-debug-guidelines)