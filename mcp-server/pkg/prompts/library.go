// Copyright 2026 k8s-gpu-mcp-server contributors
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

package prompts

// Library contains all built-in prompt definitions.
var Library = []PromptDef{
	GPUHealthCheck,
	DiagnoseXIDErrors,
	GPUTriage,
}

// GPUHealthCheck provides comprehensive GPU health assessment.
var GPUHealthCheck = PromptDef{
	Name:        "gpu-health-check",
	Description: "Comprehensive GPU health assessment with recommendations",
	Arguments: []ArgumentDef{
		{
			Name:        "node",
			Description: "Optional: specific node name to check (default: all nodes)",
			Required:    false,
			Default:     "all nodes",
		},
	},
	Template: `## GPU Health Check Request

Please perform a comprehensive GPU health assessment on {{node}}.

### Workflow

1. **Inventory Check**
   - Use the ` + "`get_gpu_inventory`" + ` tool to list all GPUs
   - Note GPU models, memory sizes, and current utilization

2. **Health Assessment**
   - Use the ` + "`get_gpu_health`" + ` tool to get health scores
   - Check temperature, power, memory, and ECC status
   - Flag any GPUs with health score below 90

3. **Analysis**
   - Identify any thermal throttling (temperature > 80°C)
   - Check for memory pressure (> 90% utilization)
   - Review any health warnings or recommendations

### Expected Output

Provide a summary including:
- Total GPU count and models
- Overall cluster GPU health status
- Any GPUs requiring attention
- Specific recommendations for remediation`,
}

// DiagnoseXIDErrors provides XID error analysis workflow.
var DiagnoseXIDErrors = PromptDef{
	Name:        "diagnose-xid-errors",
	Description: "Analyze NVIDIA XID errors from kernel logs with remediation guidance",
	Arguments: []ArgumentDef{
		{
			Name:        "time_range",
			Description: "Time range to analyze (e.g., '1h', '24h', '7d')",
			Required:    false,
			Default:     "24h",
		},
	},
	Template: `## XID Error Diagnosis Request

Please analyze NVIDIA XID errors from the last {{time_range}}.

### Workflow

1. **Error Collection**
   - Use the ` + "`analyze_xid_errors`" + ` tool to parse kernel logs
   - Collect all XID errors with timestamps

2. **Error Classification**
   - Group errors by XID code
   - Identify severity levels (info, warning, critical)
   - Note affected GPU indices

3. **Root Cause Analysis**

   For each error type found, provide:
   - XID code meaning and typical causes
   - Whether it indicates hardware vs software issue
   - Correlation with workload patterns if visible

4. **Remediation Guidance**

   Based on findings, recommend:
   - Immediate actions (drain node, restart workload, etc.)
   - Monitoring changes needed
   - Escalation criteria (when to replace hardware)

### XID Quick Reference

| XID | Severity | Meaning |
|-----|----------|---------|
| 13  | Warning  | Graphics Engine Exception |
| 31  | Critical | GPU memory page fault |
| 43  | Warning  | GPU stopped processing |
| 45  | Critical | Preemptive cleanup |
| 48  | Critical | Double Bit ECC Error (DBE) |
| 63  | Warning  | ECC page retirement/row remap |
| 64  | Critical | ECC page retirement failed |
| 74  | Warning  | NVLink error |
| 79  | Warning  | GPU fallen off bus |
| 94  | Critical | Contained ECC error |
| 95  | Critical | Uncontained ECC error |

### Expected Output

Provide:
- Summary of errors found (count by XID, by GPU)
- Timeline of error occurrence
- Risk assessment (can workloads continue safely?)
- Concrete next steps`,
}

// GPUTriage provides standard SRE triage procedure.
var GPUTriage = PromptDef{
	Name:        "gpu-triage",
	Description: "Standard GPU triage workflow: inventory → health → XID analysis",
	Arguments: []ArgumentDef{
		{
			Name:        "node",
			Description: "Node name to triage (required for targeted diagnosis)",
			Required:    false,
			Default:     "cluster-wide",
		},
		{
			Name:        "incident_id",
			Description: "Optional incident/ticket ID for tracking",
			Required:    false,
			Default:     "",
		},
	},
	Template: `## GPU Triage Report

**Target:** {{node}}
**Incident:** {{incident_id}}

### Step 1: Hardware Inventory

Use ` + "`get_gpu_inventory`" + ` to collect:
- GPU count and models
- Memory capacity and current usage
- Temperature and power readings
- Current utilization

### Step 2: Health Assessment

Use ` + "`get_gpu_health`" + ` to evaluate:
- Health scores for each GPU
- Temperature status (normal/warning/critical)
- Power consumption vs limits
- ECC error counts (correctable/uncorrectable)
- Memory health status

### Step 3: Error Analysis

Use ` + "`analyze_xid_errors`" + ` to check:
- Recent XID errors in kernel logs
- Error frequency and patterns
- Correlation with specific GPUs

### Step 4: Workload Correlation (if K8s available)

Use ` + "`get_pod_gpu_allocation`" + ` to identify:
- Pods currently using GPUs
- Resource requests vs actual usage
- Any pods in error state

### Triage Decision Matrix

| Condition | Action |
|-----------|--------|
| All GPUs healthy, no XID errors | ✅ No action needed |
| Health score < 90, no critical errors | ⚠️ Monitor closely |
| XID 48/64/95 detected | 🔴 Drain node, escalate |
| GPU fallen off bus (XID 79) | 🔴 Immediate node restart |
| Thermal throttling (>83°C) | ⚠️ Check cooling, reduce load |
| Memory errors accumulating | ⚠️ Schedule maintenance |

### Expected Output

Provide a triage report including:
1. **Summary:** One-line status (healthy/degraded/critical)
2. **Findings:** Key observations from each step
3. **Risk Assessment:** Can workloads continue safely?
4. **Recommendations:** Prioritized action items
5. **Escalation:** If needed, who to contact and why`,
}

// GetPromptByName returns a prompt definition by name.
func GetPromptByName(name string) (*PromptDef, bool) {
	for i := range Library {
		if Library[i].Name == name {
			return &Library[i], true
		}
	}

	return nil, false
}

// GetAllPromptNames returns the names of all available prompts.
func GetAllPromptNames() []string {
	names := make([]string, len(Library))
	for i, p := range Library {
		names[i] = p.Name
	}

	return names
}
