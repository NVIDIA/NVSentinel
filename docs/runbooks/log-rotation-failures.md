### Runbook: Log Rotation Failures

#### Symptoms

- Prometheus alert: `LogRotationFailures` or `FileServerLowDiskSpace`
- Metric `fileserver_log_rotation_failed_total` increasing
- Metric `fileserver_disk_space_free_bytes` decreasing to critical levels
- Old log files not being deleted
- File server pod disk full

#### Diagnosis Steps

##### 1. Check Cleanup Service Status

```bash
# Get file server pod
FILE_SERVER_POD=$(kubectl get pods -n nvsentinel -l app.kubernetes.io/name=incluster-file-server -o jsonpath='{.items[0].metadata.name}')

# Check if cleanup container is running
kubectl get pod -n nvsentinel $FILE_SERVER_POD -o jsonpath='{.spec.containers[*].name}'
# Should include: nginx, log-cleanup, (optionally) nginx-log-exporter

# Check cleanup container logs
kubectl logs -n nvsentinel $FILE_SERVER_POD -c log-cleanup --tail=100
```

##### 2. Check Disk Usage

```bash
# Check disk space on file server
kubectl exec -n nvsentinel $FILE_SERVER_POD -- df -h /usr/share/nginx/html

# Check PVC status
kubectl get pvc -n nvsentinel
kubectl describe pvc -n nvsentinel nvsentinel-incluster-file-server-data

# Count and size of log files
kubectl exec -n nvsentinel $FILE_SERVER_POD -- sh -c "find /usr/share/nginx/html -type f | wc -l"
kubectl exec -n nvsentinel $FILE_SERVER_POD -- du -sh /usr/share/nginx/html/*
```

##### 3. Check Rotation Configuration

```bash
# Get current configuration
kubectl get cm -n nvsentinel file-server-log-cleanup-config -o yaml

# Verify cleanup container environment variables
kubectl get pod -n nvsentinel $FILE_SERVER_POD -o jsonpath='{.spec.containers[?(@.name=="log-cleanup")].env[*]}' | jq
```

##### 4. Check Metrics

```bash
# Port-forward to cleanup metrics endpoint
kubectl port-forward -n nvsentinel $FILE_SERVER_POD 9002:9002

# Query cleanup metrics
curl http://localhost:9002/metrics | grep fileserver_log_rotation
curl http://localhost:9002/metrics | grep fileserver_disk_space
```

#### Common Failure Causes and Solutions

##### Issue 1: Cleanup Container Not Running

**Error**: Cleanup container is missing or crashed

**Solution**:
```bash
# Check pod events
kubectl describe pod -n nvsentinel $FILE_SERVER_POD

# Check if cleanup is enabled in Helm values
helm get values -n nvsentinel nvsentinel -o yaml | grep -A 5 logCleanup

# If disabled, enable it:
# In values.yaml:
inclusterFileServer:
  logCleanup:
    enabled: true
    retentionDays: 7

# Upgrade Helm release
helm upgrade nvsentinel -n nvsentinel ./distros/kubernetes/nvsentinel -f values.yaml
```

##### Issue 2: Permission Errors

**Error in logs**:
```text
[ERROR] Cleanup operation failed: Permission denied
find: '/usr/share/nginx/html/upload': Permission denied
```

**Cause**: Incorrect file permissions or security context

**Solution**:
```bash
# Check file permissions
kubectl exec -n nvsentinel $FILE_SERVER_POD -- ls -la /usr/share/nginx/html

# Fix permissions if needed
kubectl exec -n nvsentinel $FILE_SERVER_POD -- chmod -R 775 /usr/share/nginx/html

# Verify init container ran successfully
kubectl logs -n nvsentinel $FILE_SERVER_POD -c init-perms
```

##### Issue 3: Timeout During Cleanup

**Error in logs**:
```text
[ERROR] Cleanup operation timed out
```

**Cause**: Too many files to process within timeout period

**Solution**:
```bash
# Check number of files
kubectl exec -n nvsentinel $FILE_SERVER_POD -- find /usr/share/nginx/html -type f | wc -l

# Manually clean very old logs first
kubectl exec -n nvsentinel $FILE_SERVER_POD -- find /usr/share/nginx/html -type f -mtime +30 -delete

# Restart cleanup container
kubectl delete pod -n nvsentinel $FILE_SERVER_POD
# (Pod will be recreated by deployment)
```

##### Issue 4: Incorrect Retention Configuration

**Error**: Files not being deleted at expected age

**Solution**:
```bash
# Check current retention setting
kubectl get cm -n nvsentinel file-server-log-cleanup-config -o yaml | grep LOG_RETENTION_DAYS

# Update retention period
# In Helm values:
inclusterFileServer:
  logCleanup:
    retentionDays: 3  # Reduce from 7 to 3 days

# Apply changes
helm upgrade nvsentinel -n nvsentinel ./distros/kubernetes/nvsentinel -f values.yaml

# Force immediate cleanup cycle
kubectl exec -n nvsentinel $FILE_SERVER_POD -c log-cleanup -- pkill -HUP python3
```

##### Issue 5: Disk Full - Emergency Response

**Critical**: Disk is full and blocking new uploads

**Immediate action**:
```bash
# 1. Manually delete oldest logs
kubectl exec -n nvsentinel $FILE_SERVER_POD -- find /usr/share/nginx/html -type f -mtime +1 -delete

# 2. Delete logs for specific nodes that are no longer relevant
kubectl exec -n nvsentinel $FILE_SERVER_POD -- rm -rf /usr/share/nginx/html/<old-node-name>

# 3. Check space recovered
kubectl exec -n nvsentinel $FILE_SERVER_POD -- df -h /usr/share/nginx/html

# 4. Increase PVC size (if storage class supports it)
kubectl patch pvc -n nvsentinel nvsentinel-incluster-file-server-data \
  -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'

# 5. Verify expansion
kubectl get pvc -n nvsentinel -w
```

##### Issue 6: Cleanup Running But Files Not Deleted

**Error**: Cleanup reports success but files remain

**Solution**:
```bash
# Test find command manually
kubectl exec -n nvsentinel $FILE_SERVER_POD -- find /usr/share/nginx/html -type f -mtime +7 -ls

# Check if files are actually older than retention period
kubectl exec -n nvsentinel $FILE_SERVER_POD -- find /usr/share/nginx/html -type f -printf '%T+ %p\n' | sort | head -20

# Force cleanup with debug
kubectl exec -n nvsentinel $FILE_SERVER_POD -c log-cleanup -- sh -c "
  echo 'Running manual cleanup...'
  find /usr/share/nginx/html -type f -mtime +7 -print -delete
"
```

#### Resolution Steps

1. **Immediate triage**:
   ```bash
   # Check if disk is critically low (< 5GB)
   DISK_FREE=$(kubectl exec -n nvsentinel $FILE_SERVER_POD -- df -BG /usr/share/nginx/html | tail -1 | awk '{print $4}' | sed 's/G//')
   if [ "$DISK_FREE" -lt 5 ]; then
     echo "CRITICAL: Immediate cleanup needed"
     # Run emergency cleanup
     kubectl exec -n nvsentinel $FILE_SERVER_POD -- find /usr/share/nginx/html -type f -mtime +1 -delete
   fi
   ```

2. **Fix root cause** using solutions above

3. **Verify cleanup is working**:
   ```bash
   # Monitor cleanup metrics
   kubectl port-forward -n nvsentinel $FILE_SERVER_POD 9002:9002 &
   watch -n 10 'curl -s http://localhost:9002/metrics | grep fileserver_log_rotation'
   
   # Watch disk space
   watch -n 30 'kubectl exec -n nvsentinel $FILE_SERVER_POD -- df -h /usr/share/nginx/html'
   ```

4. **Test with a dry run**:
   ```bash
   # See what would be deleted
   kubectl exec -n nvsentinel $FILE_SERVER_POD -- \
     find /usr/share/nginx/html -type f -mtime +7 -ls
   ```

5. **Force cleanup cycle**:
   ```bash
   # Trigger cleanup without waiting for next interval
   kubectl exec -n nvsentinel $FILE_SERVER_POD -c log-cleanup -- pkill -HUP python3
   
   # Watch logs
   kubectl logs -n nvsentinel $FILE_SERVER_POD -c log-cleanup -f
   ```

---

