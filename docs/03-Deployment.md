# 🚀 NVSentinel Deployment Guide

Welcome to the NVSentinel deployment guide! This document outlines the end-to-end deployment process that ensures safe, coordinated rollouts of NVSentinel across all production environments. The process emphasizes safety, communication, and proper change management.

## 📋 Quick Overview

The deployment process has been **largely automated using Argo workflows** and serves as the controlled rollout mechanism for NVSentinel updates across all production clusters. This guide documents the automated flow and provides manual intervention points to ensure safe deployments. The process includes comprehensive pre-deployment checks, maintenance coordination, and phased rollouts across all cloud service providers.

**🤖 Automation Note:** Most steps in this process are automated through our Argo workflow pipelines. This documentation serves as a reference for understanding the end-to-end flow and for manual oversight during critical deployment phases.

---

## 🎯 Deployment Workflow

### 1. 🧪 Verify Qualification Completion

**🚨 CRITICAL PREREQUISITE:** Deployment **MUST NOT** proceed without completed and signed-off qualification.

**Qualification Requirements:**
- ✅ Sprint qualification **COMPLETED** as per the [Qualification Guide](./02-Qualification.md)
- ✅ Final qualification report **POSTED** in #dgxc-gpu-node-resilience
- ✅ All critical and high-severity bugs **RESOLVED** or **MITIGATED**

**🛑 DEPLOYMENT BLOCKER:** If qualification is **NOT completed** or **NOT signed off**, deployment is **STRICTLY PROHIBITED**. No exceptions.

**Verification Checklist:**
```
□ Qualification report shows "PASSED" or "PASSED WITH ISSUES"
□ No critical bugs remain unresolved
□ No outstanding qualification concerns
```

**💡 If qualification is incomplete:** Work with the team to complete qualification first. Do not attempt to shortcut this process.

### 2. 📋 Create Maintenance Documentation (T-2 Days)

**Timeline:** **2 days prior** to the planned deployment date.

Create a comprehensive maintenance document using our established template and approval process.

**Documentation Requirements:**

**Template Location:** [MaintenanceTemplate.md](https://gitlab-master.nvidia.com/dgxcloud/mk8s/docs/-/blob/main/docs/07-Maintenances/nvsentinel/MaintenanceTemplate.md)

**Reference Examples:** Check the same directory for previous maintenance examples to understand the expected format and detail level.

**MR Creation Process:**
```bash
# Create your maintenance documentation MR
git checkout main
git pull origin main
git checkout -b <username>/nvsentinel-maintenance-<date>

# Copy and customize the template
cp MaintenanceTemplate.md nvsentinel-maintenance-<YYYY-MM-DD>.md
# Edit with deployment-specific details

git add .
git commit -m "docs: add NVSentinel maintenance doc for <date>"
git push origin <branch-name>
```

**Required Approvers:**
- ✅ **Neeraj Kapoor** - MANDATORY approval required
- ✅ **Vipul Sabhia** - MANDATORY approval required

**🚨 DEPLOYMENT BLOCKER:** The deployment **CANNOT proceed** without both required approvals on the maintenance documentation MR.

**Approval Process:**
- Tag both approvers in the MR
- Engage them in the **#dgxc-gpu-node-resilience** Slack channel if needed
- Provide clear deployment scope and impact assessment
- Be responsive to any feedback or questions

### 3. 🔧 Open Rootly Maintenance Window

**Prerequisite:** Maintenance documentation MR **APPROVED and MERGED**.

Create a Rootly maintenance event with proper scheduling and documentation.

**Rootly Configuration:**
- ✅ **Start Time:** Exact deployment window start
- ✅ **End Time:** Conservative estimate with buffer time
- ✅ **Runbook:** Link to the merged maintenance documentation

### 4. 📊 Update Deployment Tracking and Notify Stakeholders

Add the maintenance event to our tracking system and notify key stakeholders.

**Tracking Requirements:**

**Spreadsheet:** [Deployment Tracking Sheet](https://docs.google.com/spreadsheets/d/1H1AsXGwNi6-YbSfIpKb6ELREnuns6nj3OQDw1Kq8K58/edit?pli=1&gid=0#gid=0)

**Required Information:**
- Deployment date and time
- NVSentinel version being deployed
- Rootly maintenance ID
- Expected duration
- Contact person (deployment lead)

**Stakeholder Notification:**

**Notify Shaun Fox** in **#dgxc-gpu-node-resilience**:
```
🚀 NVSENTINEL DEPLOYMENT SCHEDULED 🚀

Date: <deployment-date>
Time: <start-time> - <end-time>
Version: <nvsentinel-version>
Rootly: <maintenance-link>

Tracking sheet updated: [link]

@shaun.fox - FYI deployment scheduled as above
```

### 5. 📢 Notify CSE Team Members

Inform all Customer Support Engineering team members about the upcoming deployment.

**Notification in #dgxc-gpu-node-resilience:**
```
📅 UPCOMING NVSENTINEL DEPLOYMENT 📅

Hi CSE team! 👋

Scheduled Deployment:
Date: <deployment-date>
Time: <start-time> - <end-time> UTC
Version: NVSentinel <version>

What to expect:
✅ Rolling updates across all clusters
✅ Minimal customer impact expected
✅ Real-time updates will be posted during deployment

Rootly: <maintenance-link>
Runbook: <documentation-link>

Please route any related customer inquiries to the maintenance team during this window.

@cse-team-members
```

### 6. ⚙️ Pre-Deployment MR Generation (T-1 Day)

**Timeline:** **1 day before** the maintenance window.

Kick off the automated Argo workflow to generate deployment MRs for all target clusters.

**Workflow Execution:**
```bash
# Trigger the Argo workflow for MR generation
# This is typically done through the GitLab CI/CD interface
# or via API calls to the Argo workflow controller
```

**🔍 MR Spot Check Requirements:**

**CRITICAL:** Manually review generated MRs to ensure **ONLY intended changes** are included.

**Spot Check Checklist:**
```
□ Only NVSentinel version updates present
□ No unexpected configuration changes
□ No additional helm chart modifications
□ Consistent changes across all cluster MRs
```

**🚨 STOP CONDITIONS:** If ANY unexpected changes are detected:
1. **IMMEDIATELY halt** the deployment process
2. **Investigate** the source of unexpected changes
3. **Fix and regenerate** MRs to remove unintended modifications
4. **Re-run spot checks** before proceeding

**Action if Issues Found:**
```
🛑 DEPLOYMENT PAUSED 🛑

Issue: Unexpected changes detected in generated MRs
Action: Investigating and fixing MR generation process
Status: Deployment on hold until resolution

Will provide update once fixed.
```

### 7. 🚀 Execute Phased Deployment

**Timeline:** During the scheduled maintenance window.

Execute the deployment in controlled batches with monitoring and validation at each phase.

**Deployment Strategy:**
- **Batch Size:** Deploy to cluster batches (typically 1-5 clusters per batch)
- **Wait Time:** Wait for ArgoCD "Healthy" status before proceeding to next batch
- **Validation:** Verify pod health and functionality after each batch

**Batch Deployment Process:**

1. Merge MRs for the batch
2. Monitor ArgoCD applications for "Synced" and "Healthy" status
3. Verify all pods are "Running" 
4. Check for any error logs or alerts
5. Proceed to next batch only after successful validation


**Validation Criteria per Batch:**
- ✅ ArgoCD application status: **"Healthy"**
- ✅ All NVSentinel pods: **"Running"**
- ✅ No error alerts or notifications
- ✅ Health monitor endpoints responding
- ✅ No resource constraint issues

**🚨 Rollback Triggers:**
- Any pods failing to start
- ArgoCD showing "Degraded" status
- Critical alerts or monitoring failures
- Resource exhaustion issues

### 8. 📱 Maintain Communication During Deployment

Provide regular updates in the Rootly maintenance Slack channel throughout the deployment.

**Update Frequency:** After each batch completion or every 30 minutes (whichever is more frequent).

**Progress Update Template:**
```
🔄 DEPLOYMENT PROGRESS UPDATE

Time: <current-time>
Batch: <current-batch> of <total-batches>
Status: <IN_PROGRESS/COMPLETED/PAUSED>

✅ Completed Clusters: <count>
⏳ In Progress: <count>  
⚠️ Issues: <count> (if any)

Next Batch: <cluster-names>
ETA: <estimated-time>

All systems showing healthy status ✅
```

**Issue Communication:**
```
⚠️ DEPLOYMENT ISSUE DETECTED

Issue: <brief-description>
Affected Clusters: <cluster-names>
Impact: <assessment>
Action: <immediate-response>

Investigating... Will update in 30 minutes.
```

### 9. ✅ Complete Deployment and Final Notifications

Once all clusters are successfully updated, finalize the deployment process.

**Final Validation:**
- ✅ All target clusters show **"Healthy"** ArgoCD status
- ✅ All NVSentinel pods across all clusters are **"Running"**
- ✅ No critical alerts or error notifications
- ✅ Health monitor endpoints responding across all CSPs

**Team Notification in #dgxc-gpu-node-resilience:**
```
🎉 NVSENTINEL DEPLOYMENT COMPLETED SUCCESSFULLY 🎉

Deployment Summary:
Version: NVSentinel <version>
Clusters: <total-count> clusters across AWS/GCP/Azure
Duration: <actual-duration>
Issues: <none/resolved-issues>

✅ All clusters reporting healthy status
✅ All pods running successfully  
✅ No critical alerts
✅ Customer impact: None detected

Rootly maintenance marked as completed.

Great work team! 🚀
```

**Rootly Completion:**
- Mark maintenance as **"Completed"**
- Add final summary with actual duration and outcomes
- Close the maintenance Slack channel or post final status

**Post-Deployment Monitoring:**
- Monitor clusters for 24 hours post-deployment
- Watch for any delayed issues or performance impacts
- Be ready to respond to any customer reports

---

## 🆘 Getting Help

**For deployment issues or questions:**

1. **Primary Channel:** #dgxc-gpu-node-resilience Slack channel
2. **Escalation:** Page the on-call team via Rootly
3. **Emergency:** Direct contact with team leads Neeraj Kapoor or Vipul Sabhia

**Remember:** When in doubt, communicate early and often. The team is here to support successful deployments! 🤝

---

**Happy deploying! 🎉**

*This guide is a living document. If you encounter edge cases or have process improvements, please update this guide or create a JIRA ticket for enhancements.*
