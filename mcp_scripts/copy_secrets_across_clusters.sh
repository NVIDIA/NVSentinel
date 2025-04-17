# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

PRODUCER_KUBECONFIG="/home/kdabhadkar/.kube/config-msti07-dgxc-runai-gcp-cmh-dev0-mgmt"
CONSUMER_KUBECONFIG="/home/kdabhadkar/.kube/config-ldit01-dgxc-runai-gcp-cmh-dev0"
NAMESPACE="dgxc-system"

# Create namespace and pull secret on Producer cluster
echo "Creating namespace and pull secret on Producer cluster..."
kubectl --kubeconfig="${PRODUCER_KUBECONFIG}" create ns "${NAMESPACE}" --dry-run=client -o yaml | kubectl --kubeconfig="${PRODUCER_KUBECONFIG}" apply -f -
kubectl --kubeconfig="${PRODUCER_KUBECONFIG}" label namespace "${NAMESPACE}" app.kubernetes.io/managed-by=Helm --overwrite
kubectl --kubeconfig="${PRODUCER_KUBECONFIG}" annotate namespace "${NAMESPACE}" meta.helm.sh/release-name="${NAMESPACE}" --overwrite
kubectl --kubeconfig="${PRODUCER_KUBECONFIG}" annotate namespace "${NAMESPACE}" meta.helm.sh/release-namespace="${NAMESPACE}" --overwrite

#Please make sure to have this pull secret created in this namespace
# kubectl --kubeconfig="${PRODUCER_KUBECONFIG}" apply -f - <<EOF
# apiVersion: v1
# data:
#   .dockerconfigjson: <pull-secret-base64-encoded-string>
# kind: Secret
# metadata:
#   name: nvidia-ngcuser-pull-secret
#   namespace: ${NAMESPACE}
# type: kubernetes.io/dockerconfigjson
# EOF

# Create namespace and pull secret on Consumer cluster
echo "Creating namespace and pull secret on Consumer cluster..."
kubectl --kubeconfig="${CONSUMER_KUBECONFIG}" create ns "${NAMESPACE}" --dry-run=client -o yaml | kubectl --kubeconfig="${CONSUMER_KUBECONFIG}" apply -f -
kubectl --kubeconfig="${CONSUMER_KUBECONFIG}" label namespace "${NAMESPACE}" app.kubernetes.io/managed-by=Helm --overwrite
kubectl --kubeconfig="${CONSUMER_KUBECONFIG}" annotate namespace "${NAMESPACE}" meta.helm.sh/release-name="${NAMESPACE}" --overwrite
kubectl --kubeconfig="${CONSUMER_KUBECONFIG}" annotate namespace "${NAMESPACE}" meta.helm.sh/release-namespace="${NAMESPACE}" --overwrite

#Please make sure to have this pull secret created in this namespace
# kubectl --kubeconfig="${CONSUMER_KUBECONFIG}" apply -f - <<EOF
# apiVersion: v1
# data:
#   .dockerconfigjson: <pull-secret-base64-encoded-string>
# kind: Secret
# metadata:
#   name: nvidia-ngcuser-pull-secret
#   namespace: ${NAMESPACE}
# type: kubernetes.io/dockerconfigjson
# EOF

echo "Copying mongo-app-client-cert-secret..."
kubectl --kubeconfig="${PRODUCER_KUBECONFIG}" get secret mongo-app-client-cert-secret -n "${NAMESPACE}" -o json \
| jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.managedFields, .metadata.selfLink)' \
| kubectl --kubeconfig="${CONSUMER_KUBECONFIG}" apply -n "${NAMESPACE}" -f -


echo "Copying mongo-ca-secret..."
kubectl --kubeconfig="${PRODUCER_KUBECONFIG}" get secret mongo-ca-secret -n "${NAMESPACE}" -o json \
| jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.managedFields, .metadata.selfLink)' \
| kubectl --kubeconfig="${CONSUMER_KUBECONFIG}" apply -n "${NAMESPACE}" -f -


echo "Copying mongo-root-ca-secret..."
kubectl --kubeconfig="${PRODUCER_KUBECONFIG}" get secret mongo-root-ca-secret -n "${NAMESPACE}" -o json \
| jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.managedFields, .metadata.selfLink)' \
| kubectl --kubeconfig="${CONSUMER_KUBECONFIG}" apply -n "${NAMESPACE}" -f -

echo "Secret copy process completed."