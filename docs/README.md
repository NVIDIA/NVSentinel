# NVSentinel Documentation

Welcome! This documentation will help you understand, configure, and operate NVSentinel - an intelligent monitoring and self-healing system for GPU-enabled Kubernetes clusters.

## 📚 Documentation Structure

### [OVERVIEW.md](./OVERVIEW.md) - Start Here
**Read this first** (5 min read)  
A plain-English introduction to what NVSentinel is, the problems it solves, and how it works. Perfect for understanding the big picture.

### [INTEGRATIONS.md](./INTEGRATIONS.md) - How to Use NVSentinel
**Essential for platform engineers** (10 min read)  
Learn how to integrate NVSentinel with your scheduling, monitoring, and remediation systems. Covers taints, node conditions, custom remediation triggers, and drain behavior customization.

### [DATA_FLOW.md](./DATA_FLOW.md) - Architecture Deep Dive
**For developers and architects** (15 min read)  
Understand how data flows through the system - from GPU failure detection to automated remediation. Includes sequence diagrams and data transformation details.

### [METRICS.md](./METRICS.md) - Observability Reference
**For SREs and monitoring teams** (10 min reference)  
Complete catalog of all Prometheus metrics exposed by NVSentinel components. Use this to build dashboards and alerts.

### [tutorials/](./tutorials/)
**Step-by-step developer guides**  
Hands-on walkthroughs for extending NVSentinel:

- [Writing a New Health Monitor](./tutorials/writing-a-health-monitor.md) — build, deploy, and
  verify a custom fault detector end-to-end (no GPU required).
- [Writing a Preflight Check](./tutorials/writing-a-preflight-check.md) — build an init-container
  diagnostic, register it in Helm, and verify it blocks bad GPU pod starts.
- [Writing a Drain Plugin](./tutorials/writing-a-drain-plugin.md) — replace node-drainer's
  eviction step with a custom controller.
- [Plugin Custom Remediation](./tutorials/plugin-custom-remediation.md) — extend the
  fault-remediation stage with your own repair via custom actions and a controller CRD.

### [configuration/](./configuration/)
**Component configuration guides**  
Detailed setup instructions for each NVSentinel component. Read these when you need to configure specific monitors, connectors, or remediation modules.

### [runbooks/](./runbooks/)
**Troubleshooting and operations**  
Step-by-step guides for diagnosing and resolving common operational issues. Your go-to resource when something isn't working as expected.

### [designs/](./designs/)
**Technical design documents**  
Architecture decisions and design rationales for major features. Useful if you're contributing to the project or need to understand implementation choices.

## 🔧 Component Documentation

Individual component feature documentation:

- [GPU Health Monitor](./gpu-health-monitor.md)
- [Syslog Health Monitor](./syslog-health-monitor.md)
- [CSP Health Monitor IAM Setup](./csp-health-monitor-iam.md)
- [Fault Quarantine](./fault-quarantine.md)
- [Node Drainer](./node-drainer.md)
- [Fault Remediation](./fault-remediation.md)
- [Kubernetes Object Monitor](./kubernetes-object-monitor.md)
- [Circuit Breaker](./circuit-breaker.md)
- [Cancelling Breakfix](./cancelling-breakfix.md)
- [Event Exporter](./event-exporter.md)
- [Metadata Collector](./metadata-collector.md)
- [Labeler](./labeler.md)
- [Log Collection](./log-collection.md)
- [Platform Connectors](./platform-connectors.md)
- [Preflight](./preflight.md)
- [State Manager](./state-manager.md)
