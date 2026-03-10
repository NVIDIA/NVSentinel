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

set -euox pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/../common.sh"

CLUSTER_PREFIX="${CLUSTER_PREFIX:-nvs}"
CLUSTER_NAME="${CLUSTER_NAME:-}"
AWS_REGION="${AWS_REGION:-us-east-1}"
STACK_DELETE_TIMEOUT="${STACK_DELETE_TIMEOUT:-600}"

main() {
    log "Starting EKS cluster cleanup..."
    log "  CLUSTER_PREFIX: $CLUSTER_PREFIX"
    log "  CLUSTER_NAME:   ${CLUSTER_NAME:-<derived from stacks>}"
    log "  AWS_REGION:     $AWS_REGION"

    # Determine which cluster(s) to clean up
    local cluster_names=""

    if [ -n "$CLUSTER_NAME" ]; then
        cluster_names="$CLUSTER_NAME"
    else
        # Derive cluster names from existing CloudFormation stacks
        local stacks
        stacks=$(aws cloudformation list-stacks \
            --region "$AWS_REGION" \
            --query "StackSummaries[?starts_with(StackName, 'eksctl-${CLUSTER_PREFIX}-') && StackStatus!='DELETE_COMPLETE'].StackName" \
            --output text)

        if [ -z "$stacks" ]; then
            log "No CloudFormation stacks found with prefix: $CLUSTER_PREFIX"
            exit 0
        fi

        cluster_names=$(get_cluster_names_from_stacks "$stacks")
    fi

    if [ -z "$cluster_names" ]; then
        log "No clusters to clean up."
        exit 0
    fi

    log "Clusters to clean up: $cluster_names"

    # Clean up each cluster
    local failed=0
    for cluster_name in $cluster_names; do
        log "========================================="
        log "Cleaning up cluster: $cluster_name"
        log "========================================="

        # Step 1: Delete OIDC provider (before cluster is gone)
        delete_oidc_provider "$cluster_name" || {
            log "WARNING: OIDC cleanup failed for $cluster_name; continuing with stack deletion"
            failed=1
        }

        # Step 2: Delete CloudFormation stacks and GPU subnets in dependency order
        # (nodegroups first, then GPU subnets, then addons, then cluster/VPC)
        if ! delete_cluster_stacks "$cluster_name"; then
            failed=1
        fi
    done

    log "========================================="
    if [ "$failed" -ne 0 ]; then
        log "Cluster cleanup completed with errors"
        exit 1
    fi
    log "All cluster cleanups completed"
    log "========================================="
}

# Extract unique cluster names from CloudFormation stack names.
# eksctl stacks follow the pattern: eksctl-{CLUSTER_NAME}-{suffix}
# where suffix is "cluster", "nodegroup-*", or "addon-*".
get_cluster_names_from_stacks() {
    local stacks="$1"
    local cluster_names=""

    # Stack names follow: eksctl-{CLUSTER}-cluster, eksctl-{CLUSTER}-nodegroup-{NG},
    # eksctl-{CLUSTER}-addon-{ADDON}. We strip the eksctl- prefix and known
    # suffixes. Note: shortest-suffix removal (single %) will still truncate
    # cluster names that themselves contain "-nodegroup-" or "-addon-"; this is
    # safe in practice because NVSentinel cluster names never contain those
    # substrings.
    for stack in $stacks; do
        local name
        name="${stack#eksctl-}"
        name="${name%-cluster}"
        name="${name%-nodegroup-*}"
        name="${name%-addon-*}"
        cluster_names="${cluster_names:+${cluster_names} }${name}"
    done

    # Return unique names
    echo "$cluster_names" | tr ' ' '\n' | sort -u | grep -v '^$' | tr '\n' ' ' | sed 's/ $//'
}

# Delete the IAM OIDC provider associated with an EKS cluster.
# If the cluster is already gone, falls back to orphan detection.
delete_oidc_provider() {
    local cluster_name="$1"

    log "Cleaning up OIDC provider for cluster: $cluster_name..."

    # Get the OIDC issuer URL from the EKS cluster
    local oidc_issuer
    oidc_issuer=$(aws eks describe-cluster \
        --name "$cluster_name" \
        --region "$AWS_REGION" \
        --query 'cluster.identity.oidc.issuer' \
        --output text 2>/dev/null) || {
        log "WARNING: Could not describe cluster $cluster_name (may already be deleted). Searching for orphaned OIDC providers..."
        delete_orphaned_oidc_providers "$cluster_name"
        return $?
    }

    if [[ -z "$oidc_issuer" || "$oidc_issuer" == "None" ]]; then
        log "No OIDC issuer found for cluster $cluster_name"
        return 0
    fi

    # Extract the OIDC ID from the issuer URL (last path segment)
    local oidc_id
    oidc_id="${oidc_issuer##*/}"

    # Find the matching IAM OIDC provider
    local provider_arn
    provider_arn=$(aws iam list-open-id-connect-providers \
        --query "OpenIDConnectProviderList[?ends_with(Arn, '/${oidc_id}')].Arn" \
        --output text 2>/dev/null) || {
        log "WARNING: Failed to list OIDC providers when looking for issuer $oidc_issuer"
        return 0
    }

    if [[ -z "$provider_arn" || "$provider_arn" == "None" ]]; then
        log "No OIDC provider found matching issuer: $oidc_issuer"
        return 0
    fi

    log "Deleting OIDC provider: $provider_arn"
    aws iam delete-open-id-connect-provider \
        --open-id-connect-provider-arn "$provider_arn" || log "WARNING: Failed to delete OIDC provider $provider_arn"

    log "OIDC provider cleanup complete for cluster $cluster_name"
}

# Search for and delete orphaned OIDC providers when the cluster
# is already gone but the provider was left behind.
delete_orphaned_oidc_providers() {
    local cluster_name="$1"

    log "Searching for orphaned OIDC providers (triggered by cluster: ${cluster_name})..."

    # List all OIDC providers and find ones associated with EKS in this region
    local provider_arns
    provider_arns=$(aws iam list-open-id-connect-providers \
        --query 'OpenIDConnectProviderList[].Arn' \
        --output text 2>/dev/null) || {
        log "WARNING: Could not list OIDC providers"
        return 0
    }

    # Get list of active clusters once (avoid repeated API calls in the loop).
    # If this fails, we must not proceed — an empty list would cause us to
    # incorrectly treat every OIDC provider as orphaned and delete it.
    local active_clusters
    active_clusters=$(aws eks list-clusters \
        --region "$AWS_REGION" \
        --query 'clusters[]' \
        --output text 2>/dev/null) || {
        log "ERROR: Failed to list active EKS clusters in ${AWS_REGION}; aborting orphaned OIDC cleanup to avoid deleting providers for active clusters"
        return 1
    }

    # Precompute OIDC IDs for all active clusters to avoid repeated API calls
    local active_oidc_ids=""
    for cluster in $active_clusters; do
        local cluster_oidc
        if ! cluster_oidc=$(aws eks describe-cluster \
            --name "$cluster" \
            --region "$AWS_REGION" \
            --query 'cluster.identity.oidc.issuer' \
            --output text 2>/dev/null); then
            log "ERROR: Failed to describe cluster '${cluster}' in ${AWS_REGION}; aborting orphaned OIDC cleanup to avoid deleting providers for active clusters"
            return 1
        fi

        if [ -z "$cluster_oidc" ] || [ "$cluster_oidc" = "None" ]; then
            log "ERROR: Could not obtain OIDC issuer for cluster '${cluster}' in ${AWS_REGION}; aborting orphaned OIDC cleanup to avoid deleting providers for active clusters"
            return 1
        fi

        active_oidc_ids="${active_oidc_ids} ${cluster_oidc##*/}"
    done

    for arn in $provider_arns; do
        # EKS OIDC providers have URLs like: oidc.eks.{region}.amazonaws.com/id/{ID}
        local provider_url
        provider_url=$(aws iam get-open-id-connect-provider \
            --open-id-connect-provider-arn "$arn" \
            --query 'Url' \
            --output text 2>/dev/null) || continue

        if echo "$provider_url" | grep -q "oidc.eks.${AWS_REGION}.amazonaws.com"; then
            local oidc_id
            oidc_id="${provider_url##*/}"

            local is_orphaned=true
            for active_id in $active_oidc_ids; do
                if [ "$active_id" = "$oidc_id" ]; then
                    is_orphaned=false
                    break
                fi
            done

            if [ "$is_orphaned" = true ]; then
                log "Deleting orphaned OIDC provider: $arn (URL: $provider_url)"
                aws iam delete-open-id-connect-provider \
                    --open-id-connect-provider-arn "$arn" || log "WARNING: Failed to delete orphaned OIDC provider $arn"
            fi
        fi
    done
}

# Delete CloudFormation stacks and GPU subnets for a cluster in dependency order:
# nodegroups -> GPU subnets -> addons/IAM -> cluster (VPC).
delete_cluster_stacks() {
    local cluster_name="$1"

    log "Deleting CloudFormation stacks for cluster: $cluster_name..."

    # Categorize stacks by type for ordered deletion
    local all_stacks
    all_stacks=$(aws cloudformation list-stacks \
        --region "$AWS_REGION" \
        --query "StackSummaries[?starts_with(StackName, 'eksctl-${cluster_name}-') && StackStatus!='DELETE_COMPLETE'].StackName" \
        --output text 2>/dev/null) || {
        log "WARNING: Could not list stacks for cluster $cluster_name"
        return 1
    }

    if [ -z "$all_stacks" ]; then
        log "No CloudFormation stacks found for cluster $cluster_name"
        # Still clean up GPU subnets — they were created outside CloudFormation
        delete_gpu_subnets "$cluster_name"
        return 0
    fi

    local nodegroup_stacks=""
    local addon_stacks=""
    local cluster_stack=""

    for stack in $all_stacks; do
        case "$stack" in
            *-cluster)
                cluster_stack="$stack"
                ;;
            *-nodegroup-*)
                nodegroup_stacks="${nodegroup_stacks} ${stack}"
                ;;
            *-addon-*)
                addon_stacks="${addon_stacks} ${stack}"
                ;;
            *)
                addon_stacks="${addon_stacks} ${stack}"
                ;;
        esac
    done

    local failed=0

    # Phase 1: nodegroup stacks (must be deleted before GPU subnets or cluster stack)
    if [ -n "$nodegroup_stacks" ]; then
        log "Phase 1: Deleting nodegroup stacks..."
        if ! delete_stacks_and_wait "$nodegroup_stacks"; then
            log "WARNING: One or more nodegroup stack deletions failed for cluster $cluster_name"
            failed=1
        fi
    fi

    # Phase 2: GPU subnets (safe now that nodegroup instances are terminated,
    # must happen before cluster stack deletes the VPC)
    if ! delete_gpu_subnets "$cluster_name"; then
        log "WARNING: GPU subnet cleanup had failures for cluster $cluster_name"
        failed=1
    fi

    # Phase 3: addon/IAM stacks
    if [ -n "$addon_stacks" ]; then
        log "Phase 3: Deleting addon/IAM stacks..."
        if ! delete_stacks_and_wait "$addon_stacks"; then
            log "WARNING: One or more addon/IAM stack deletions failed for cluster $cluster_name"
            failed=1
        fi
    fi

    # Phase 4: cluster stack (must be last — deletes VPC)
    if [ -n "$cluster_stack" ]; then
        log "Phase 4: Deleting cluster stack..."
        if ! delete_stacks_and_wait "$cluster_stack"; then
            log "WARNING: Cluster stack deletion failed for cluster $cluster_name"
            failed=1
        fi
    fi

    log "CloudFormation stack cleanup complete for cluster $cluster_name"
    return "$failed"
}

# Delete manually-created GPU subnets for a cluster.
# Cleans up route table associations and ENIs before deleting.
# Called by: delete_cluster_stacks() — Phase 2
delete_gpu_subnets() {
    local cluster_name="$1"
    local exit_code=0

    log "Cleaning up GPU subnets for cluster: $cluster_name..."

    # Find subnets tagged with kubernetes.io/cluster/{CLUSTER_NAME}=owned
    local subnet_ids
    subnet_ids=$(aws ec2 describe-subnets \
        --region "$AWS_REGION" \
        --filters "Name=tag:kubernetes.io/cluster/${cluster_name},Values=owned" \
        --query 'Subnets[].SubnetId' \
        --output text 2>/dev/null) || {
        log "WARNING: Could not query subnets for cluster $cluster_name"
        return 1
    }

    if [[ -z "$subnet_ids" || "$subnet_ids" == "None" ]]; then
        log "No tagged subnets found for cluster $cluster_name"
        return 0
    fi

    for subnet_id in $subnet_ids; do
        log "Deleting subnet: $subnet_id"

        # Disassociate any route table associations (except the main one)
        local associations
        associations=$(aws ec2 describe-route-tables \
            --region "$AWS_REGION" \
            --filters "Name=association.subnet-id,Values=$subnet_id" \
            --query 'RouteTables[].Associations[?SubnetId==`'"$subnet_id"'`].RouteTableAssociationId' \
            --output text 2>/dev/null) || true

        for assoc_id in $associations; do
            if [[ -n "$assoc_id" && "$assoc_id" != "None" ]]; then
                log "Disassociating route table: $assoc_id"
                if ! aws ec2 disassociate-route-table \
                    --region "$AWS_REGION" \
                    --association-id "$assoc_id" 2>/dev/null; then
                    log "WARNING: Failed to disassociate route table $assoc_id"
                    exit_code=1
                fi
            fi
        done

        # Delete any network interfaces in the subnet
        local eni_ids
        eni_ids=$(aws ec2 describe-network-interfaces \
            --region "$AWS_REGION" \
            --filters "Name=subnet-id,Values=$subnet_id" \
            --query 'NetworkInterfaces[].NetworkInterfaceId' \
            --output text 2>/dev/null) || true

        for eni_id in $eni_ids; do
            if [[ -n "$eni_id" && "$eni_id" != "None" ]]; then
                # Detach ENI if still attached (e.g. from recently terminated instances)
                local attachment_id
                attachment_id=$(aws ec2 describe-network-interfaces \
                    --region "$AWS_REGION" \
                    --network-interface-ids "$eni_id" \
                    --query 'NetworkInterfaces[0].Attachment.AttachmentId' \
                    --output text 2>/dev/null) || true

                if [[ -n "$attachment_id" && "$attachment_id" != "None" ]]; then
                    log "Detaching network interface: $eni_id (attachment: $attachment_id)"
                    if ! aws ec2 detach-network-interface \
                        --region "$AWS_REGION" \
                        --attachment-id "$attachment_id" \
                        --force 2>/dev/null; then
                        log "WARNING: Failed to detach ENI $eni_id"
                        exit_code=1
                    fi
                    if ! wait_for_eni_detach "$eni_id"; then
                        exit_code=1
                    fi
                fi

                log "Deleting network interface: $eni_id"
                if ! aws ec2 delete-network-interface \
                    --region "$AWS_REGION" \
                    --network-interface-id "$eni_id" 2>/dev/null; then
                    log "WARNING: Failed to delete ENI $eni_id"
                    exit_code=1
                fi
            fi
        done

        # Delete the subnet
        if ! aws ec2 delete-subnet \
            --region "$AWS_REGION" \
            --subnet-id "$subnet_id"; then
            log "WARNING: Failed to delete subnet $subnet_id"
            exit_code=1
        fi
    done

    log "GPU subnet cleanup complete for cluster $cluster_name"
    return "$exit_code"
}

# Poll until an ENI has no attachment or reaches "available" status,
# or until the timeout expires.
wait_for_eni_detach() {
    local eni_id="$1"
    local timeout=60
    local interval=5
    local elapsed=0

    while [ "$elapsed" -lt "$timeout" ]; do
        local eni_status
        eni_status=$(aws ec2 describe-network-interfaces \
            --region "$AWS_REGION" \
            --network-interface-ids "$eni_id" \
            --query 'NetworkInterfaces[0].Status' \
            --output text 2>/dev/null) || break

        if [[ "$eni_status" == "available" || -z "$eni_status" || "$eni_status" == "None" ]]; then
            return 0
        fi

        sleep "$interval"
        elapsed=$((elapsed + interval))
    done

    if [ "$elapsed" -ge "$timeout" ]; then
        log "WARNING: Timed out waiting for ENI $eni_id to detach after ${timeout}s; proceeding with delete attempt"
    fi

    return 0
}

# Delete a list of stacks and wait for all of them to finish.
# Returns 0 if all succeeded, 1 if any failed.
delete_stacks_and_wait() {
    local stacks="$1"
    local failed=0

    if [ -z "$stacks" ]; then
        return 0
    fi

    # Initiate deletion
    for stack in $stacks; do
        log "Deleting stack: $stack"
        aws cloudformation delete-stack \
            --region "$AWS_REGION" \
            --stack-name "$stack" || log "WARNING: Failed to initiate deletion of $stack"
    done

    # Wait for all to complete
    for stack in $stacks; do
        if ! wait_for_stack_deletion "$stack"; then
            failed=1
        fi
    done

    return "$failed"
}

# Wait for a CloudFormation stack to finish deleting.
# Returns 0 on success, 1 on timeout/failure.
wait_for_stack_deletion() {
    local stack_name="$1"
    local timeout="${2:-$STACK_DELETE_TIMEOUT}"
    local elapsed=0
    local interval=15

    log "Waiting for stack deletion: $stack_name (timeout: ${timeout}s)..."

    while [ "$elapsed" -lt "$timeout" ]; do
        local status
        status=$(aws cloudformation describe-stacks \
            --region "$AWS_REGION" \
            --stack-name "$stack_name" \
            --query 'Stacks[0].StackStatus' \
            --output text 2>/dev/null) || {
            log "Stack $stack_name no longer exists (deleted successfully)"
            return 0
        }

        case "$status" in
            DELETE_COMPLETE)
                log "Stack $stack_name deleted successfully"
                return 0
                ;;
            DELETE_FAILED)
                local failure_reason
                failure_reason=$(aws cloudformation describe-stack-events \
                    --region "$AWS_REGION" \
                    --stack-name "$stack_name" \
                    --query "StackEvents[?ResourceStatus=='DELETE_FAILED'] | [0].ResourceStatusReason" \
                    --output text 2>/dev/null) || failure_reason="<unable to retrieve>"
                log "WARNING: Stack $stack_name deletion failed: ${failure_reason}"
                return 1
                ;;
            DELETE_IN_PROGRESS)
                ;;
            *)
                log "WARNING: Stack $stack_name in unexpected state: $status"
                ;;
        esac

        sleep "$interval"
        elapsed=$((elapsed + interval))
    done

    log "WARNING: Timed out waiting for stack $stack_name deletion after ${timeout}s"
    return 1
}

main "$@"
