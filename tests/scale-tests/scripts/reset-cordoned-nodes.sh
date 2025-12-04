#!/bin/bash
# Reset all NVSentinel-cordoned nodes (BULK operations for large clusters)

echo "🔄 Resetting NVSentinel-cordoned nodes..."

# 1. Restart FQM FIRST to clear in-memory state
echo "   Restarting FQM..."
kubectl rollout restart deployment/fault-quarantine -n nvsentinel

# 2. While FQM restarts, bulk uncordon all nodes with NVSentinel cordon label
echo "   Bulk uncordoning nodes..."
kubectl get nodes -l k8saas.nvidia.com/cordon-by=NVSentinel -o name | xargs -r kubectl uncordon

# 3. Bulk remove labels
echo "   Removing NVSentinel labels..."
kubectl label nodes -l k8saas.nvidia.com/cordon-by=NVSentinel \
    k8saas.nvidia.com/cordon-by- \
    k8saas.nvidia.com/cordon-reason- \
    k8saas.nvidia.com/cordon-timestamp- \
    dgxc.nvidia.com/nvsentinel-state- 2>/dev/null || true

# 4. NOW wait for FQM to be ready
echo "   Waiting for FQM rollout..."
kubectl rollout status deployment/fault-quarantine -n nvsentinel --timeout=60s

# 5. Verify
REMAINING=$(kubectl get nodes -l k8saas.nvidia.com/cordon-by=NVSentinel --no-headers 2>/dev/null | wc -l)
echo ""
echo "✅ Reset complete. Remaining cordoned: $REMAINING"
echo ""
echo "⚠️  REMINDER: You may also need to wipe MongoDB state manually!"
echo "   Use: scripts/mongodb-shell.sh to connect and clear HealthEvents collection"
