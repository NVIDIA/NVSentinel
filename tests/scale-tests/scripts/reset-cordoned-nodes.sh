#!/bin/bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Reset all NVSentinel-cordoned nodes (BULK operations for large clusters)

echo "🔄 Resetting NVSentinel-cordoned nodes..."

# 1. Scale FQM to 0 to stop it while we clean up
echo "   Scaling FQM to 0..."
kubectl scale deployment/fault-quarantine -n nvsentinel --replicas=0
kubectl scale deployment/node-drainer -n nvsentinel --replicas=0
kubectl scale deployment/fault-remediation -n nvsentinel --replicas=0

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

# 4. Wait before scaling back up
echo "   Waiting 60 seconds before scaling back up..."
sleep 60

# 5. Scale deployments back to 1
echo "   Scaling deployments back to 1..."
kubectl scale deployment/node-drainer -n nvsentinel --replicas=1
kubectl scale deployment/fault-quarantine -n nvsentinel --replicas=1
kubectl scale deployment/fault-remediation -n nvsentinel --replicas=1

# 6. Verify
REMAINING=$(kubectl get nodes -l k8saas.nvidia.com/cordon-by=NVSentinel --no-headers 2>/dev/null | wc -l)
echo ""
echo "✅ Reset complete. Remaining cordoned: $REMAINING"
echo ""
echo "⚠️  REMINDER: You may also need to wipe MongoDB state manually!"
echo "   Use: scripts/mongodb-shell.sh to connect and clear HealthEvents collection"
