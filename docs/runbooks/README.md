# NVSentinel Runbooks

This directory contains troubleshooting runbooks for NVSentinel operations.

## Available Runbooks

### Core System Issues
- [Circuit Breaker Troubleshooting](circuit-breaker.md) - Reset a tripped circuit breaker after mass cordon events
- [Cordoned Nodes](cordoned-nodes.md) - Investigate and remediate cordoned nodes
- [Node Conditions](node-conditions.md) - Handle nodes with health conditions but not cordoned
- [Stale Events](stale-events.md) - Clear accumulated events after system downtime
- [Datastore Connection](datastore-connection.md) - Connect to MongoDB for health event queries
- [Driver Upgrades](driver-upgrades.md) - Runbook for driver upgrade

### Platform Connector Issues
- [Node Event Creation Failures](node-event-creation-failures.md) - Resolve failures when creating Kubernetes node events
- [Node Condition Update Failures](node-condition-update-failures.md) - Fix failures updating node health conditions

### Health Monitor Issues
- [Health Monitor UDS Communication Failures](health-monitor-uds-failures.md) - Troubleshoot health monitor to platform-connector communication
- [GPU Monitor DCGM Connectivity Failures](gpu-monitor-dcgm-failures.md) - Resolve DCGM connectivity issues
- [CSP Health Monitor Issues](csp-health-monitor.md) - Troubleshoot cloud provider API access and permissions

### Operational Issues
- [Log Collection Job Failures](log-collection-job-failures.md) - Troubleshoot failed log collection jobs
- [Log Rotation Failures](log-rotation-failures.md) - Troubleshoot log rotation and cleanup issues
