# Runbook: Health Event Analyzer High Error Rate

## Symptoms

**Prometheus Alert:** `HealthEventAnalyzerHighErrorRate`

**Metric:** High ratio of `health_event_analyzer_event_processing_errors` to `health_event_analyzer_events_received_total`

**Common Causes:**
1. Malformed MongoDB aggregation queries in the health-events-analyzer configuration
2. MongoDB connection errors (network issues, service unavailable, authentication failures)

## Overview

The Health Events Analyzer (HEA) processes health events from MongoDB and applies rules defined in the `health-events-analyzer-config` ConfigMap. These rules contain MongoDB aggregation pipeline queries. Errors can occur due to:
- Syntax errors in the aggregation pipeline queries
- Typos in field names (e.g., `generatedtimestap` instead of `generatedtimestamp`)
- Invalid MongoDB operators (e.g., `$notequal` instead of `$ne`)
- MongoDB connectivity issues

## Diagnosis Steps

### Issue 1: Malformed Database Queries in Configuration

The Health Events Analyzer executes rules defined in the configuration file. These rules contain queries written in MongoDB aggregation pipeline syntax. Typos or syntax errors will cause processing failures every time a rule is evaluated for processing event.

**Diagnosis:**

```bash
# Check for query syntax errors in logs
kubectl logs -n nvsentinel deployment/health-events-analyzer --tail=200 | \
  grep -E "Failed to execute aggregation pipeline|Failed to parse stage"

# Common error patterns to look for:
# - "Unrecognized pipeline stage name" → Invalid MongoDB operator (e.g., $notequal)
# - "field not found" → Typo in field name (e.g., generatedtimestap)
# - "parse error" → JSON syntax error in the query
```

**Solution:**

```bash
# 1. Edit the configuration to fix the query syntax
kubectl -n nvsentinel edit configmap health-events-analyzer-config

# 2. Restart the deployment to pick up the new configuration
kubectl -n nvsentinel rollout restart deployment/health-events-analyzer

# 3. Wait for the new pod to be ready
kubectl -n nvsentinel rollout status deployment/health-events-analyzer

# 4. Verify the fix by monitoring logs for errors
kubectl -n nvsentinel logs deployment/health-events-analyzer -f | \
  grep -E "Failed to execute|Failed to parse stage"

# If no errors appear, the fix was successful
```

### Issue 2: MongoDB Connection Error

The Health Events Analyzer establishes a connection to MongoDB to listen for inserted events and to publish new health events. Connection errors prevent event processing and increase health_event_analyzer_event_processing_errors metric for `execute_pipeline_error` error type.

Follow the [MongoDB Connection Error runbook](https://github.com/NVIDIA/NVSentinel/blob/main/docs/runbooks/mongodb-connection-error.md) to diagnose and resolve MongoDB connection issues.

If MongoDB pods are in a healthy state, check for connectivity errors in the health-events-analyzer pod:

```bash
# Check for connection errors in HEA logs
kubectl -n nvsentinel logs deployment/health-events-analyzer --tail=100 | \
  grep -iE "connection refused|connection timeout|dial tcp.*mongodb|failed to connect to mongodb|mongo.*error"

# If you see errors like:
# - "connection refused" → MongoDB service is not available
# - "connection timeout" → Network issues or MongoDB not responding
# - "authentication failed" → MongoDB credentials issue
# - "no such host" → MongoDB service DNS resolution failing

# re-establish the connection by restarting the pod
kubectl -n nvsentinel rollout restart deployment/health-events-analyzer

# verify the connection is established
kubectl -n nvsentinel logs deployment/health-events-analyzer -f | \
  grep -iE "connection|mongodb|connected|ready"

# Look for messages like:
# - "Successfully pinged database to confirm connectivity" → Success
# - Continuing to see connection errors → Investigate further (check service, network policies, credentials)
```