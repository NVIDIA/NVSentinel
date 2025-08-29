# 🧪 NVSentinel Qualification Guide

Welcome to the NVSentinel qualification guide! This document outlines the end-to-end qualification process that runs at the end of each sprint to validate all deliverables and ensure system reliability across our supported environments.

## 📋 Quick Overview

The qualification process has been **largely automated using Argo workflows** and serves as the final quality gate before sprint completion. This guide documents the automated flow and provides manual intervention points when needed. The process ensures that all NVSentinel components work correctly across different cloud providers and cluster configurations.

**🤖 Automation Note:** Most steps in this process are automated through our Argo workflow pipelines. This documentation serves as a reference for understanding the end-to-end flow and for manual intervention when automation requires human oversight.

---

## 🎯 Qualification Workflow

### 1. 🚀 Deploy Latest NVSentinel Version

The qualification process begins by deploying the latest version of NVSentinel across all development clusters.

**Target Clusters:**
- All dev clusters listed in the [Dev Cluster Inventory](https://docs.google.com/spreadsheets/d/1EdZIW8kjYEop8SJ8xh6ITcIltB4lr0z7w2498qtSwzc/edit?gid=0#gid=0)
- Includes clusters across **all CSPs:** AWS, GCP, Azure
- Covers various GPU configurations and cluster sizes

**Deployment Process:**
```bash
# The automated workflow deploys using ArgoCD
# Manual verification can be done with:
kubectl get applications -n argocd nvsentinel
```

**🔍 Health Check Requirements:**
- ✅ ArgoCD application shows **"Healthy"** status
- ✅ All NVSentinel pods are in **"Running"** state
- ✅ No crashing pods in the cluster
- ✅ No driver issues or GPU-related errors
- ✅ All core components (platform-connector, health monitors, etc.) are operational

**🚨 CRITICAL:** Testing **MUST NOT** proceed if there are any:
- Crashing pods in the cluster
- Unhealthy ArgoCD applications  
- Driver failures or GPU accessibility issues
- Resource constraints affecting pod scheduling

### 2. 🏃 Execute End-to-End Test Pipeline

Once all clusters are healthy, the automated end-to-end test pipeline is triggered.

**GitLab Pipeline Details:**
- **Repository:** `dgxcloud/mk8s/autotest`
- **Branch:** `main`
- **Test Suite:** `k8s-platform/functional`
- **Pipeline Variables:**
  - `ENVIRONMENT`: "non-prod"
  - `API_ENVIRONMENT`: "dev"
  - `CLUSTER_TYPE`: "MK8S"
  - `CLUSTER_NAME`: Target cluster path
  - `RUN_AUTOTEST`: "true"
  - `TEST_SUITE`: "k8s-platform/functional"
  - `AUTOTEST_LOG_LEVEL`: "default"

**Pipeline Execution:**
- GitLab CI/CD pipeline triggered for each target cluster
- Parallel execution across multiple clusters
- Each cluster gets its own pipeline instance

**Test Coverage:**
- End-to-end GPU health monitoring workflows
- Node draining and remediation processes
- Event processing and persistence validation
- Cross-CSP compatibility verification

### 3. ⏱️ Monitor Pipeline Completion

Wait for the automated test pipeline to complete across all target clusters.

**Monitoring Requirements:**
- Wait for the pipeline to finish
- Monitor for any infrastructure failures during execution

**Expected Duration:**
- End-to-end test suite typically takes **1-2 hours** per cluster
- Total qualification time depends on cluster count and parallel execution

### 4. 📊 Analyze Test Results and Handle Flaky Tests

Review test results and identify any failures that require investigation.

**🎯 Known Issue - Flaky Tests:**
Our test suite contains some inherently flaky tests. **ALL failing tests should be rerun once** before being classified as genuine bugs.

**Flaky Test Handling:**
1. **First Failure:** Automatically rerun the failing test
2. **Second Failure:** Classify as a genuine bug requiring investigation
3. **Pass on Rerun:** Mark as flaky but acceptable (log for future improvement)

**Classification Criteria:**
- ✅ **Pass:** Test succeeded on first or second attempt
- ⚠️ **Flaky:** Failed first attempt, passed on rerun
- ❌ **Bug:** Failed both attempts consistently

### 5. 🐛 Create JIRA Bugs for Failing Tests

For every test that fails on both attempts, create a detailed JIRA bug report.

**Bug Creation Requirements:**

**JIRA Project:** **KACE**

**Bug Template:**
```
Title: [SPRINT-QUAL] <Test Name> - <Brief Description>

Description:
Sprint: <Sprint Number>
Test Case: <Full test case name>
Cluster(s): <Affected cluster names>
CSP: <AWS/GCP/Azure>

Pipeline Link: <Link to GitLab CI/CD pipeline execution>
Observations: <Manual observations from the person running tests>

Severity: <Critical/High/Medium/Low based on impact>
```

**Bug Severity Guidelines:**
- **Critical:** System crashes, data loss, security vulnerabilities
- **High:** Major functionality broken, affects multiple users
- **Medium:** Feature partially working, workarounds available  
- **Low:** Minor issues, cosmetic problems

### 6. 📢 Report Bugs in Slack Channel

Post each identified bug in the team Slack channel for immediate visibility.

**Slack Channel:** **#dgxc-gpu-node-resilience**

**Bug Report Format:**
```
🚨 QUALIFICATION BUG FOUND 🚨

Test: <Test Case Name>
JIRA: KACE-<number>
Severity: <Critical/High/Medium/Low>
CSP: <AWS/GCP/Azure>
Cluster: <cluster-name>

Brief Description: <One-line summary of the issue>

Details: <Link to JIRA ticket>

@dgxc-k8s-gpu-node-resilience - please investigate when possible
```

**Team Notification:**
- Tag the full team for **Critical** and **High** severity bugs
- Provide clear, actionable information
- Include all relevant context for quick triage

### 7. 📈 Send Final Qualification Report

Once all testing is complete and bugs are documented, send a comprehensive qualification report.

**Final Report Format:**

Post in **#dgxc-gpu-node-resilience**:

```
🏁 SPRINT QUALIFICATION COMPLETE 🏁

Sprint: <Sprint Number>
Qualification Date: <Date>
NVSentinel Version: <version tag>

📊 END-TO-END TEST RESULTS SUMMARY:
✅ Total Tests: <number>
✅ Passed: <number> (<percentage>%)
⚠️ Flaky (passed on rerun): <number> (<percentage>%)
❌ Failed: <number> (<percentage>%)

🌍 CLUSTER COVERAGE:
✅ AWS Clusters: <count> tested
✅ GCP Clusters: <count> tested  
✅ Azure Clusters: <count> tested
✅ Total Clusters: <count> tested

🐛 BUGS IDENTIFIED:
Critical: <count> bugs
High: <count> bugs  
Medium: <count> bugs
Low: <count> bugs

📋 JIRA TICKETS CREATED:
<List of KACE ticket numbers with brief descriptions>

🎯 QUALIFICATION STATUS: 
<PASSED/FAILED/PASSED WITH ISSUES>

<Additional notes or concerns>

Great work team! 🚀
```

**Qualification Status Guidelines:**
- **PASSED:** No critical/high bugs, minor issues documented
- **PASSED WITH ISSUES:** Some medium bugs identified but system functional
- **FAILED:** Critical bugs found, sprint deliverables blocked

---

## 🤝 Getting Help

For any assistance with qualification issues, reach out to the **#dgxc-gpu-node-resilience** Slack channel.

---

**Happy qualifying! 🎉**

*This guide is a living document. If you encounter edge cases or have process improvements, please update this guide or create a JIRA ticket for enhancements.*
