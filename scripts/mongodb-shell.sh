#!/usr/bin/env bash
#
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
#

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

NAMESPACE="nvsentinel"
POD_NAME="mongodb-0"
LOCAL_PORT="27017"
REMOTE_PORT="27017"
SECRET_NAME="mongo-app-client-cert-secret"
DATABASE="HealthEventsDatabase"
COLLECTION="HealthEvents"

print_status() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

cleanup() {
    print_status "Cleaning up..."
    
    if [ ! -z "$PF_PID" ]; then
        print_status "Stopping port forward (PID: $PF_PID)"
        kill $PF_PID 2>/dev/null || true
    fi
    
    if [ -d "$CERT_DIR" ]; then
        rm -f "$CERT_DIR/ca.crt" "$CERT_DIR/tls.crt" "$CERT_DIR/tls.key" "$CERT_DIR/creds.pem"
        print_status "Removed certificate files from $CERT_DIR"
    fi
    
    print_success "Cleanup completed"
}

trap cleanup EXIT INT TERM

if ! command -v kubectl &> /dev/null; then
    print_error "kubectl is not installed or not in PATH"
    exit 1
fi

KUBE_CONTEXT=$(kubectl config current-context)
if [ -z "$KUBE_CONTEXT" ]; then
    print_error "No current kubectl context found"
    exit 1
fi

CERT_DIR="$HOME/.nvsentinel/certs/$KUBE_CONTEXT"

if command -v mongosh &> /dev/null; then
    MONGOSH_CMD="mongosh"
else
    print_error "'mongosh' is not available"
    print_status "Install MongoDB Shell: https://www.mongodb.com/docs/mongodb-shell/install/"
    exit 1
fi

print_status "Starting MongoDB connection setup..."

if ! kubectl get namespace $NAMESPACE &> /dev/null; then
    print_error "Namespace '$NAMESPACE' not found"
    exit 1
fi

if ! kubectl get pod $POD_NAME -n $NAMESPACE &> /dev/null; then
    print_error "Pod '$POD_NAME' not found in namespace '$NAMESPACE'"
    print_status "Available MongoDB pods:"
    kubectl get pods -n $NAMESPACE -l app.kubernetes.io/name=mongodb -o name
    exit 1
fi

POD_STATUS=$(kubectl get pod $POD_NAME -n $NAMESPACE -o jsonpath='{.status.phase}')
if [ "$POD_STATUS" != "Running" ]; then
    print_error "Pod '$POD_NAME' is not running (status: $POD_STATUS)"
    exit 1
fi

print_success "MongoDB pod is running"

if [ ! -d "$CERT_DIR" ]; then
    mkdir -p "$CERT_DIR"
    chmod 700 "$CERT_DIR"
    print_status "Created certificate directory for context '$KUBE_CONTEXT': $CERT_DIR"
fi

print_status "Fetching MongoDB certificates from Kubernetes secret..."

if ! kubectl get secret $SECRET_NAME -n $NAMESPACE &> /dev/null; then
    print_error "Secret '$SECRET_NAME' not found in namespace '$NAMESPACE'"
    exit 1
fi

kubectl get secret $SECRET_NAME -n $NAMESPACE -o jsonpath='{.data.ca\.crt}' | base64 -d > "$CERT_DIR/ca.crt"
chmod 600 "$CERT_DIR/ca.crt"

kubectl get secret $SECRET_NAME -n $NAMESPACE -o jsonpath='{.data.tls\.crt}' | base64 -d > "$CERT_DIR/tls.crt"
kubectl get secret $SECRET_NAME -n $NAMESPACE -o jsonpath='{.data.tls\.key}' | base64 -d > "$CERT_DIR/tls.key"

cat "$CERT_DIR/tls.crt" "$CERT_DIR/tls.key" > "$CERT_DIR/creds.pem"
chmod 600 "$CERT_DIR/creds.pem"

print_success "MongoDB certificates extracted to $CERT_DIR"

print_status "Starting port forward from localhost:$LOCAL_PORT to $POD_NAME:$REMOTE_PORT"

kubectl port-forward $POD_NAME $LOCAL_PORT:$REMOTE_PORT -n $NAMESPACE &>/dev/null &
PF_PID=$!

sleep 3

if ! kill -0 $PF_PID 2>/dev/null; then
    print_error "Port forward failed to start"
    exit 1
fi

print_success "Port forward established successfully"

MONGODB_URI="mongodb://localhost:$LOCAL_PORT/$DATABASE?directConnection=true&authMechanism=MONGODB-X509&authSource=\$external&tls=true&tlsCAFile=$CERT_DIR/ca.crt&tlsCertificateKeyFile=$CERT_DIR/creds.pem&tlsAllowInvalidHostnames=true"

print_status "Connecting to MongoDB..."
print_status "Database: $DATABASE"
print_status "Collection: $COLLECTION"
echo ""

print_status "Starting MongoDB shell..."
echo "Type 'exit' to quit"
echo ""

$MONGOSH_CMD "$MONGODB_URI"

print_success "MongoDB shell session completed"
