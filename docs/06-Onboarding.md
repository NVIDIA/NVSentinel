# 🚀 NVSentinel New Developer Onboarding Guide

Welcome to the NVSentinel team! 🎉 This document will guide you through everything you need to get up and running as a productive member of our GPU node resilience team. We're excited to have you aboard and look forward to working together to build amazing technology!

## 📋 Quick Overview

NVSentinel is a GPU node resilience system that monitors and automatically remediates hardware and software faults in Kubernetes GPU clusters. As a new team member, you'll be working with a modular microservices architecture that includes health monitors, core processing modules, and integration components across multiple cloud service providers (AWS, GCP, Azure).

This onboarding guide will walk you through all the essential steps to get you productive quickly, from accessing our systems to understanding our codebase and development processes.

---

## 🎯 Onboarding Checklist

### 1. 🔐 Get System Access

**🚨 CRITICAL FIRST STEPS:** Complete these access requests immediately - they can take time to process!

#### Join Required DLGroups

**GitLab Developer Access:**
- ✅ **Join DLGroup:** `access-gitlab-dgxcloud-k8s-apps-developers`
- ✅ **Purpose:** Provides developer access to the GitLab repository
- ✅ **How to Join:** Submit request through NVIDIA's internal DLGroup system

**Project Updates Access:**
- ✅ **Join DLGroup:** `DGXCloud-Top5` 
- ✅ **Purpose:** Stay informed about project progress and associated projects
- ✅ **How to Join:** Submit request through NVIDIA's internal DLGroup system

### 2. 📚 Review Core Documentation

#### System Architecture Deep Dive

**🚨 MANDATORY READING:** Review and understand the System Architecture and Design Document (SADD).

- ✅ **Document Link:** [NVSentinel SADD](https://docs.google.com/document/d/1OlfISc0GC6ojenKz-EZvsn11ex3aCGSW4Yp79B8bgOc/edit?tab=t.0)
- ✅ **What to Focus On:** 
  - Overall system architecture
  - Component interactions
  - Data flow patterns
  - Integration points
- ✅ **Action Required:** Ask questions in **#dgxc-gpu-node-resilience** Slack channel

**Questions to Consider While Reading:**
- How do health monitors communicate with the platform connector?
- What triggers the core modules to process events?
- How does the system handle failures and recovery?
- What are the key integration points with Kubernetes?

### 3. 👥 Join Team Communications

#### Get Added to Daily Standup

- ✅ **Contact:** **Neeraj Kapoor** directly to be added to the team standup

#### Join Slack Channels

**Primary Team Channel:**
- ✅ **Channel:** **#dgxc-gpu-node-resilience**
- ✅ **Purpose:** Main team communication, questions, updates, deployments
- ✅ **Usage:** Daily communication, asking questions, sharing updates

### 4. 🎫 Access Project Management Tools

#### JIRA Board Access

**Project Tracking:**
- ✅ **JIRA Board:** [KACE Project Board](https://jirasw.nvidia.com/secure/RapidBoard.jspa?rapidView=37688&projectKey=KACE#)
- ✅ **Project Key:** **KACE**
- ✅ **What to Explore:**
  - Current sprint backlog
  - Active tickets and assignments
  - Sprint planning patterns
  - Ticket templates and workflows

**💡 Getting Familiar with JIRA:**
- Browse recent completed tickets to understand work patterns
- Look at ticket descriptions to understand requirements format
- Review acceptance criteria examples
- Understand our labeling and prioritization system

### 5. 📊 Access Monitoring and Dashboards

#### Grafana Dashboards

**System Monitoring:**
- ✅ **Dashboard Location:** [NVSentinel Dashboards](https://nvidia.grafana.net/dashboards/f/felniza0okl4we/breakfix-automation)
- ✅ **What to Explore:**
  - System health metrics
  - Performance dashboards
  - Alert configurations
  - Historical data patterns

**💡 Dashboard Familiarization:**
- Understand key system metrics
- Learn what "normal" system behavior looks like
- Identify critical alerts and thresholds
- Bookmark frequently used dashboards

### 6. 🗂️ Access Key Resources and Documentation

#### Cluster Information

**Dev Cluster Inventory:**
- ✅ **Spreadsheet:** [Dev Cluster Inventory](https://docs.google.com/spreadsheets/d/1EdZIW8kjYEop8SJ8xh6ITcIltB4lr0z7w2498qtSwzc/edit?gid=0#gid=0)
- ✅ **Contains:** All development clusters across AWS, GCP, Azure
- ✅ **Usage:** Understanding test environments and deployment targets

#### Repository Access

**Main Repository:**
- ✅ **Location:** `dgxcloud/mk8s/k8s-addons/nvsentinel`
- ✅ **Access Required:** GitLab DLGroup membership (step 1)
- ✅ **Initial Actions:**
  - Clone the repository
  - Explore the codebase structure
  - Read the main README.md
  - Review recent merge requests

### 7. 💻 Set Up Development Environment

#### Required Tools Installation

**Essential Development Tools:**

**Docker with BuildX Support:**
```bash
# Install Docker Desktop or Docker Engine
# Ensure BuildX is enabled for multi-platform builds
docker buildx version
```

**Code Editor:**
- ✅ **Recommended:** **VSCode** or **Cursor**
- ✅ **Required Extensions:**
  - Go extension
  - Python extension

**Go Development:**
```bash
# Install Go (latest stable version)
go version

# Install useful Go tools
go install golang.org/x/tools/cmd/goimports@latest
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

**Python Development:**
```bash
# Install Poetry for Python dependency management
curl -sSL https://install.python-poetry.org | python3 -

# Verify installation
poetry --version
```

**Kubernetes Tools:**
```bash
# Install kubectl
kubectl version --client

# Install Helm
helm version

# Install kind for local testing (optional but recommended)
kind version
```

**Git Configuration:**
```bash
# Configure Git with your NVIDIA email
git config --global user.name "Your Name"
git config --global user.email "your.email@nvidia.com"

# Set up SSH key for GitLab (recommended)
ssh-keygen -t ed25519 -C "your.email@nvidia.com"
```

### 8. 📖 Read Team Documentation

#### Essential Reading Order

**Start with these documents in order:**
1. ✅ **[01-Development.md](./01-Development.md)** - Complete development workflow
2. ✅ **[02-Qualification.md](./02-Qualification.md)** - End-to-end testing process
3. ✅ **[03-Deployment.md](./03-Deployment.md)** - Production deployment process
4. ✅ **[04-GraduationCriteria.md](./04-GraduationCriteria.md)** - Feature rollout process
5. ✅ **[05-Security.md](./05-Security.md)** - Security requirements and approval processes

**💡 Reading Strategy:**
- Don't try to memorize everything at once
- Focus on understanding the overall processes
- Bookmark sections you'll reference frequently
- Ask questions in Slack as you read

---

**Welcome aboard! 🎉**

*This onboarding guide is a living document. If you find steps that could be clearer or additional information that would help future new hires, please update this guide as part of your first contribution!*
