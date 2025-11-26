# CSP Health Monitor Configuration

## Overview

The CSP Health Monitor detects cloud provider maintenance events and triggers automated node quarantine workflows. This document covers all Helm configuration options.

## Module Enable/Disable

Controls whether the csp-health-monitor module is deployed in the cluster.

```yaml
global:
  cspHealthMonitor:
    enabled: true
```

## Cloud Provider Selection

The `cspName` field determines which cloud provider to monitor. Only one provider can be active at a time.

```yaml
csp-health-monitor:
  cspName: "gcp"  # Options: "gcp" or "aws"
```

## Global Settings

Settings that apply regardless of cloud provider.

```yaml
csp-health-monitor:
  logLevel: info  # Options: debug, info, warn, error
  
  configToml:
    # Cluster identifier used in health events
    clusterName: "my-cluster"
    
    # How often the sidecar polls MongoDB for maintenance events (seconds)
    maintenanceEventPollIntervalSeconds: 60
    
    # Minutes before maintenance start time to trigger quarantine
    triggerQuarantineWorkflowTimeLimitMinutes: 30
    
    # Minutes after maintenance ends to send healthy event
    postMaintenanceHealthyDelayMinutes: 15
    
    # Timeout for node to become ready after maintenance (minutes)
    nodeReadinessTimeoutMinutes: 60
```

## GCP Configuration

### Required Fields

```yaml
csp-health-monitor:
  cspName: "gcp"
  
  configToml:
    clusterName: "my-gke-cluster"
    
    gcp:
      # GCP project ID where the cluster runs
      targetProjectId: "my-gcp-project-id"
      
      # GCP Service Account name (without @project.iam.gserviceaccount.com)
      # Must match the GCP SA created in IAM setup
      gcpServiceAccountName: "csp-health-monitor"
      
      # How often to poll Cloud Logging API (seconds)
      apiPollingIntervalSeconds: 60
      
      # Cloud Logging filter for maintenance events
      logFilter: 'logName="projects/my-gcp-project-id/logs/cloudaudit.googleapis.com%2Fsystem_event" AND protoPayload.methodName="compute.instances.upcomingMaintenance"'
```

### GCP Parameters

#### targetProjectId
GCP project ID where the GKE cluster is running. The monitor queries Cloud Logging in this project.

#### gcpServiceAccountName
Name of the GCP Service Account (without the `@project.iam.gserviceaccount.com` suffix). Used to generate the Workload Identity annotation on the Kubernetes ServiceAccount.

#### apiPollingIntervalSeconds
How frequently the monitor polls the Cloud Logging API for new maintenance events. Lower values provide faster detection but increase API usage.

#### logFilter
Cloud Logging filter expression to select maintenance events. Common filters:

```python
# Standard GCE instance maintenance
'logName="projects/{PROJECT_ID}/logs/cloudaudit.googleapis.com%2Fsystem_event" AND protoPayload.methodName="compute.instances.upcomingMaintenance"'

# Include termination events
'logName="projects/{PROJECT_ID}/logs/cloudaudit.googleapis.com%2Fsystem_event" AND (protoPayload.methodName="compute.instances.upcomingMaintenance" OR protoPayload.methodName="compute.instances.terminateOnHostMaintenance")'
```

### Complete GCP Example

```yaml
global:
  cspHealthMonitor:
    enabled: true

csp-health-monitor:
  cspName: "gcp"
  logLevel: info
  
  configToml:
    clusterName: "production-gke-cluster"
    maintenanceEventPollIntervalSeconds: 60
    triggerQuarantineWorkflowTimeLimitMinutes: 30
    postMaintenanceHealthyDelayMinutes: 15
    nodeReadinessTimeoutMinutes: 60
    
    gcp:
      targetProjectId: "my-production-project"
      gcpServiceAccountName: "csp-health-monitor"
      apiPollingIntervalSeconds: 60
      logFilter: 'logName="projects/my-production-project/logs/cloudaudit.googleapis.com%2Fsystem_event" AND protoPayload.methodName="compute.instances.upcomingMaintenance"'
```

## AWS Configuration

### Required Fields

```yaml
csp-health-monitor:
  cspName: "aws"
  
  configToml:
    clusterName: "my-eks-cluster"
    
    aws:
      # AWS Account ID (12-digit number)
      accountId: "123456789012"
      
      # AWS region where the EKS cluster runs
      region: "us-east-1"
      
      # How often to poll AWS Health API (seconds)
      pollingIntervalSeconds: 60
```

### AWS Parameters

#### accountId
AWS account ID (12-digit number) where the EKS cluster is running. Used to construct the IAM role ARN annotation.

#### region
AWS region where the EKS cluster is deployed. The monitor queries the AWS Health API in this region.

#### pollingIntervalSeconds
How frequently the monitor polls the AWS Health API for maintenance events. Lower values provide faster detection but increase API usage.

### Complete AWS Example

```yaml
global:
  cspHealthMonitor:
    enabled: true

csp-health-monitor:
  cspName: "aws"
  logLevel: info
  
  configToml:
    clusterName: "production-eks-cluster"
    maintenanceEventPollIntervalSeconds: 60
    triggerQuarantineWorkflowTimeLimitMinutes: 30
    postMaintenanceHealthyDelayMinutes: 15
    nodeReadinessTimeoutMinutes: 60
    
    aws:
      accountId: "123456789012"
      region: "us-east-1"
      pollingIntervalSeconds: 60
```

## Advanced Configuration

### Out-of-Cluster Monitoring

For monitoring a tenant cluster from a separate management cluster:

```yaml
csp-health-monitor:
  configToml:
    # Path to kubeconfig for tenant cluster
    kubeconfigPath: "/etc/kubeconfig/tenant-cluster.yaml"
```

When `kubeconfigPath` is set, the monitor uses the specified kubeconfig to connect to the tenant cluster's Kubernetes API for node mapping. If empty, uses in-cluster config.

### Resources

Configure resource requests and limits for the main container and sidecar.

```yaml
csp-health-monitor:
  # Main container resources
  resources:
    limits:
      cpu: "1"
      memory: "1Gi"
    requests:
      cpu: "200m"
      memory: "256Mi"
  
  # Sidecar (Quarantine Trigger Engine) resources
  quarantineTriggerEngine:
    resources:
      limits:
        cpu: "500m"
        memory: "512Mi"
      requests:
        cpu: "100m"
        memory: "128Mi"
```

### Scheduling

Configure pod placement using node selectors, tolerations, and affinity rules.

```yaml
csp-health-monitor:
  nodeSelector:
    node-role.kubernetes.io/control-plane: ""
  
  tolerations:
    - key: "node-role.kubernetes.io/control-plane"
      operator: "Exists"
      effect: "NoSchedule"
  
  affinity: {}
```
