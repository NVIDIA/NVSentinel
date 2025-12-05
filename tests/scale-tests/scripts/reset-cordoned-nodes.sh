#!/bin/bash
# Reset all NVSentinel-cordoned nodes (BULK operations for large clusters)

echo "🔄 Resetting NVSentinel-cordoned nodes..."

# 1. Scale FQM to 0 to stop it while we clean up
echo "   Scaling FQM to 0..."
kubectl scale deployment/fault-quarantine -n nvsentinel --replicas=0

# 2. While FQM restarts, bulk uncordon ALL SchedulingDisabled nodes (parallel)
echo "   Bulk uncordoning all SchedulingDisabled nodes (parallel)..."
kubectl get nodes --no-headers | grep SchedulingDisabled | awk '{print $1}' | xargs -P 20 -I {} kubectl uncordon {}

# 3. Bulk remove labels
echo "   Removing NVSentinel labels..."
kubectl label nodes -l k8saas.nvidia.com/cordon-by=NVSentinel \
    k8saas.nvidia.com/cordon-by- \
    k8saas.nvidia.com/cordon-reason- \
    k8saas.nvidia.com/cordon-timestamp- \
    dgxc.nvidia.com/nvsentinel-state- 2>/dev/null || true

echo "Removing quarantine annotations..."
kubectl annotate nodes --all quarantineHealthEvent- quarantineHealthEventIsCordoned- 2>/dev/null || true

# 4. Scale FQM back to 1
echo "   Scaling FQM back to 1..."
kubectl scale deployment/fault-quarantine -n nvsentinel --replicas=1
kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=fault-quarantine -n nvsentinel --timeout=60s

# 5. Verify
REMAINING=$(kubectl get nodes -l k8saas.nvidia.com/cordon-by=NVSentinel --no-headers 2>/dev/null | wc -l)
echo ""
echo "✅ Reset complete. Remaining cordoned: $REMAINING"
echo ""
echo "⚠️  REMINDER: You may also need to wipe MongoDB state manually!"
echo "   Use: scripts/mongodb-shell.sh to connect and clear HealthEvents collection"
