#script to remove node conditions from all nodes in a cluster using bash script
#!/bin/bash
echo "Deleting node conditions..."
nodeConditions=("XidError" "GpuDbeMsbeProblem" "GpuHWSlowDown")
nodes=$(kubectl get nodes -o name)
for node in $nodes; do
    for condition in "${nodeConditions[@]}"; do
        conditionIndex=$(kubectl get $node -o json | jq --arg condition "$condition" '.status.conditions | map(.type == $condition) | index(true)')
        if [[ $conditionIndex != "null" ]]; then
            kubectl patch $node --type='json' -p="[{\"op\": \"remove\", \"path\": \"/status/conditions/$conditionIndex\"}]" --subresource=status
            if [[ $? -eq 0 ]]; then
                echo "Removed $condition from $node"
            else
                echo "Failed to remove $condition from $node"
            fi
        else
            echo "$condition condition not found on $node, nothing to delete."
        fi
    done
done