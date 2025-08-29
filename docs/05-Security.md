# 🔒 NVSentinel Security Best Practices Guide

Welcome to the NVSentinel security best practices guide! This document outlines the comprehensive security requirements, approval processes, and best practices that **MUST** be followed for all NVSentinel development and deployment activities. Security is everyone's responsibility and is critical to maintaining the integrity and trustworthiness of our GPU node resilience system.

## 📋 Quick Overview

Security is of **paramount importance** in NVSentinel development and operations. **Security is everyone's responsibility** and does not fall on a single individual or subteam. This guide establishes clear security requirements, mandatory approval processes, and best practices that all team members must follow. Any security-related changes require explicit approval from the designated Security PIC as documented in NSpect.

**🚨 CRITICAL:** MRs changing security-sensitive components **MUST** be approved by the Security PIC. **No exceptions.** MRs merged without security approval **WILL BE REVERTED** immediately.

---

## 🎯 Security Approval Requirements

### 🔐 Mandatory Security PIC Approval

The following types of changes **REQUIRE explicit approval** from the Security PIC before merging:

**🛡️ Security-Sensitive Changes:**

1. **Kubernetes RBAC Changes**
   - ✅ Any new or modified Kubernetes roles
   - ✅ Any new or modified role bindings
   - ✅ Service account permission changes
   - ✅ Cluster role modifications

2. **Pod Security Capabilities**
   - ✅ Any new or modified privileged capabilities
   - ✅ Security context changes
   - ✅ Pod security policy modifications
   - ✅ Container runtime security settings

3. **MongoDB Authentication**
   - ✅ Authentication mechanism changes
   - ✅ User credential modifications
   - ✅ Connection string security updates
   - ✅ Database access control changes

4. **Authentication & Authorization**
   - ✅ New authentication mechanisms
   - ✅ Authorization policy changes
   - ✅ Token management modifications
   - ✅ Access control implementations

5. **Credential Management**
   - ✅ New credentials or secrets
   - ✅ Credential rotation procedures
   - ✅ Secret storage mechanisms
   - ✅ Key management changes

6. **External Integrations**
   - ✅ New external APIs or services
   - ✅ Third-party service integrations
   - ✅ Network connectivity changes
   - ✅ Data exchange protocols

**🚨 ENFORCEMENT:** MRs containing any of these changes without Security PIC approval will be **automatically reverted** without discussion. This policy is **non-negotiable**.

---

## 🔄 Security Review Workflow

### 1. 🎫 Identify Security-Sensitive Changes

Before creating your MR, review your changes against the security requirements above.

**Self-Assessment Checklist:**
```
□ Does my change modify Kubernetes RBAC?
□ Does my change affect pod security capabilities?
□ Does my change modify authentication mechanisms?
□ Does my change involve credentials or secrets?
□ Does my change add external integrations?
□ Does my change modify authorization logic?
```

**💡 When in doubt:** Treat the change as security-sensitive and follow the approval process. It's better to be safe than sorry!

### 2. 🔍 Security Design Review (Before Implementation)

For **all security-sensitive changes**, conduct a security design review **BEFORE** implementation.

**Design Review Process:**
1. **Document the security implications** of your proposed change
2. **Create a design document** outlining security considerations
3. **Engage via the #dgxc-gpu-node-resilience Slack channel** for early feedback and guidance
4. **Iterate on the design** based on security feedback

### 3. 📝 Create Security-Compliant MR

When creating your MR, add the Security PIC as a reviewer. Look up the current Security PIC at https://nspect.nvidia.com/review?id=NSPECT-JRZ4-EI8F

### 4. 👥 Request Security PIC Review

Tag the Security PIC immediately when creating security-sensitive MRs.

**Security Review Request:**

**In MR Description:**
```
@security-pic - This MR contains security-sensitive changes and requires your explicit approval before merging. Please review when possible.

Security Change Summary: <brief description>
```

**In #dgxc-gpu-node-resilience Slack:**
```
🔒 SECURITY REVIEW REQUESTED 🔒

MR: [Link to MR]
Change Type: <RBAC/Auth/Credentials/etc>
Summary: <brief description>

@security-pic - Security approval required for merge

This MR is blocked pending security review.
```

*Note: Look up the current Security PIC at https://nspect.nvidia.com/review?id=NSPECT-JRZ4-EI8F*

### 5. ⏳ Wait for Security Approval

**🚨 CRITICAL:** Do **NOT** merge until you receive explicit Security PIC approval.

**Approval Requirements:**
- ✅ Security PIC has **explicitly approved** the MR
- ✅ All security concerns have been **addressed**
- ✅ Any required security modifications have been **implemented**

**Communication During Review:**
- Be **responsive** to security feedback
- **Address all concerns** thoroughly
- **Ask clarifying questions** if security requirements are unclear
- **Provide additional context** if requested

### 6. 🛡️ Implement Security Feedback

Address all security feedback before proceeding with merge.

**Security Feedback Response:**
- **Address every comment** from the Security PIC
- **Implement recommended changes** promptly
- **Provide explanations** for any recommendations you cannot implement
- **Re-request review** after making changes

### 7. ✅ Merge with Security Approval

Only merge after receiving explicit Security PIC approval.

**Final Merge Checklist:**

□ Security PIC has explicitly approved the MR
□ All security feedback has been addressed
□ Security-focused tests are passing
□ Documentation includes security considerations


---

## 🛡️ Security Best Practices

### 🔑 Authentication & Authorization

**Best Practices:**
- **Principle of Least Privilege:** Grant only the minimum permissions required
- **Regular Credential Rotation:** Implement automated credential rotation where possible
- **Strong Authentication:** Use multi-factor authentication for sensitive operations
- **Audit Logging:** Log all authentication and authorization events

**Implementation Guidelines:**
```go
// Example: Minimal RBAC permissions
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: nvsentinel-health-monitor
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list"]  # Only what's needed, nothing more
```

### 🏰 Pod Security

**Security Context Requirements:**
- **Non-root execution:** Run containers as non-root user
- **Read-only filesystem:** Use read-only root filesystem where possible
- **Capability dropping:** Drop all unnecessary Linux capabilities
- **Security profiles:** Apply appropriate security profiles

**Example Security Context:**
```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65534
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop:
    - ALL
```

### 🔐 Secret Management

**Secret Handling Best Practices:**
- **External Secret Stores:** Use Kubernetes secrets or external secret management systems
- **No Hardcoded Secrets:** Never include secrets in code or configuration files
- **Secret Encryption:** Ensure secrets are encrypted at rest and in transit
- **Access Auditing:** Log and monitor secret access

**Secret Usage Example:**
```go
// Good: Reading from environment variable linked to Kubernetes secret
mongoPassword := os.Getenv("MONGODB_PASSWORD")

// Bad: Hardcoded secret
// mongoPassword := "hardcoded-password" // NEVER DO THIS
```

### 🌐 Network Security

**Network Security Requirements:**
- **Network Policies:** Implement Kubernetes network policies to restrict pod-to-pod communication
- **TLS Encryption:** Use TLS for all network communications
- **Certificate Management:** Implement proper certificate lifecycle management
- **Firewall Rules:** Configure appropriate firewall rules for external communications

### 📊 Monitoring & Auditing

**Security Monitoring:**
- **Security Events:** Log all security-relevant events
- **Anomaly Detection:** Monitor for unusual access patterns
- **Regular Security Scans:** Implement automated security scanning
- **Incident Response:** Have clear incident response procedures

---

## 🆘 Getting Help

**For security questions or concerns:**

1. **Primary Channel:** #dgxc-gpu-node-resilience Slack channel
2. **Security PIC:** Look up current contact at https://nspect.nvidia.com/review?id=NSPECT-JRZ4-EI8F

**Remember:** When it comes to security, it's always better to ask questions and err on the side of caution. The team is here to help ensure we maintain the highest security standards! 🛡️

---

**Stay secure! 🔒**

*This guide is a living document. Security threats and best practices evolve continuously. Please keep this guide updated with new security requirements, lessons learned, and process improvements.*
