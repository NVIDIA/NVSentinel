### Runbook: Log Collection Job Failures

#### Symptoms

- Prometheus alert: `HighLogCollectionFailureRate`
- Failed log collector jobs visible in Kubernetes
- Missing diagnostic logs for faulted nodes
- Metric `fault_remediation_log_collector_jobs_total{status="failure"}` increasing

#### Diagnosis Steps

##### 1. Check Recent Job Status

```bash
# List recent log collector jobs
kubectl get jobs -n nvsentinel -l app=log-collector --sort-by=.metadata.creationTimestamp

# Check failed jobs
kubectl get jobs -n nvsentinel -l app=log-collector --field-selector status.successful=0
```

##### 2. Examine Job Logs

```bash
# Get the most recent failed job
FAILED_JOB=$(kubectl get jobs -n nvsentinel -l app=log-collector --field-selector status.successful=0 --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.name}')

# View job description
kubectl describe job -n nvsentinel $FAILED_JOB

# Get pod name for the failed job
FAILED_POD=$(kubectl get pods -n nvsentinel -l job-name=$FAILED_JOB -o jsonpath='{.items[0].metadata.name}')

# View pod logs
kubectl logs -n nvsentinel $FAILED_POD

# Check pod events
kubectl describe pod -n nvsentinel $FAILED_POD
```

##### 3. Check Common Issues

```bash
# Verify nvidia-driver-daemonset is running on the node
NODE_NAME=$(kubectl get job -n nvsentinel $FAILED_JOB -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="NODE_NAME")].value}')
kubectl get pods -n gpu-operator -l app=nvidia-driver-daemonset --field-selector spec.nodeName=$NODE_NAME

# Check if node is accessible
kubectl get node $NODE_NAME

# Verify file server is accessible
kubectl get svc -n nvsentinel nvsentinel-incluster-file-server
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=incluster-file-server
```

#### Common Failure Causes and Solutions

##### Issue 1: NVIDIA Driver Pod Not Found

**Error in logs**:
```text
[ERROR] nvidia-driver-daemonset pod not found on node worker-01 in namespace gpu-operator
```

**Cause**: GPU operator not deployed or driver pod not running on the node

**Solution**:
```bash
# Check GPU operator pods
kubectl get pods -n gpu-operator

# Check if GPU operator is deployed on the node
kubectl get pods -n gpu-operator -l app=nvidia-driver-daemonset --field-selector spec.nodeName=$NODE_NAME

# If missing, check node labels and taints
kubectl describe node $NODE_NAME | grep -A 5 "Labels:"
kubectl describe node $NODE_NAME | grep -A 5 "Taints:"

# Verify node has GPU
kubectl describe node $NODE_NAME | grep nvidia.com/gpu
```

##### Issue 2: Timeout Errors

**Error in logs**:
```text
[ERROR] Collection operation timed out
```

**Cause**: Log collection taking longer than configured timeout

**Solution**:
```bash
# Increase timeout in Helm values
# distros/kubernetes/nvsentinel/charts/fault-remediation/values.yaml
logCollector:
  collectionTimeout: 1800  # Increase to 30 minutes
```

##### Issue 3: Upload Failures

**Error in logs**:
```text
[UPLOAD_FAILED] Failed to upload nvidia-bug-report
curl: (7) Failed to connect to nvsentinel-incluster-file-server
```

**Cause**: File server not accessible or not running

**Solution**:
```bash
# Check file server status
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=incluster-file-server
kubectl get svc -n nvsentinel nvsentinel-incluster-file-server

# Check file server logs
FILE_SERVER_POD=$(kubectl get pods -n nvsentinel -l app.kubernetes.io/name=incluster-file-server -o jsonpath='{.items[0].metadata.name}')
kubectl logs -n nvsentinel $FILE_SERVER_POD -c nginx

# Test connectivity from log collector pod
kubectl run -n nvsentinel test-connectivity --rm -it --image=curlimages/curl --restart=Never -- \
  curl -v http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/healthz
```

##### Issue 4: Permission Errors

**Error in logs**:
```text
[ERROR] Permission denied accessing /host/home/kubernetes/bin/nvidia
```

**Cause**: Insufficient privileges or security context issues

**Solution**:
```bash
# Verify job has privileged security context
kubectl get job -n nvsentinel $FAILED_JOB -o yaml | grep -A 5 securityContext

# Should show:
# securityContext:
#   privileged: true
#   allowPrivilegeEscalation: true

# Check for PSP/PSS restrictions
kubectl get psp
kubectl get ns nvsentinel -o yaml | grep -A 5 pod-security
```

##### Issue 5: Disk Space Issues on File Server

**Error in logs**:
```text
[UPLOAD_FAILED] Failed to upload: 507 Insufficient Storage
```

**Cause**: File server persistent volume is full

**Solution**:
```bash
# Check file server disk usage
FILE_SERVER_POD=$(kubectl get pods -n nvsentinel -l app.kubernetes.io/name=incluster-file-server -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n nvsentinel $FILE_SERVER_POD -- df -h /usr/share/nginx/html

# Check PVC size
kubectl get pvc -n nvsentinel

# Option 1: Manually clean old logs
kubectl exec -n nvsentinel $FILE_SERVER_POD -- find /usr/share/nginx/html -type f -mtime +7 -delete

# Option 2: Reduce retention period
# Edit Helm values: inclusterFileServer.logCleanup.retentionDays

# Option 3: Increase PVC size (if supported by storage class)
kubectl patch pvc -n nvsentinel nvsentinel-incluster-file-server-data -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'
```

#### Resolution Steps

1. **Identify and fix the root cause** using diagnosis steps above
2. **Manually retry failed collection** (if needed):
   ```bash
   # Create a new log collection job for the node
   kubectl create job -n nvsentinel manual-log-collect-$NODE_NAME \
     --from=job/$FAILED_JOB
   ```

3. **Verify fix**:
   ```bash
   # Monitor the new job
   kubectl get jobs -n nvsentinel -l app=log-collector -w
   
   # Check logs of new job
   NEW_JOB_POD=$(kubectl get pods -n nvsentinel -l job-name=manual-log-collect-$NODE_NAME -o jsonpath='{.items[0].metadata.name}')
   kubectl logs -n nvsentinel $NEW_JOB_POD -f
   ```

4. **Verify logs are uploaded**:
   ```bash
   # Port-forward to file server
   kubectl port-forward -n nvsentinel svc/nvsentinel-incluster-file-server 8080:80
   
   # Check logs are present
   curl http://localhost:8080/$NODE_NAME/
   ```

---

