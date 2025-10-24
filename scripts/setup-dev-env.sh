#!/usr/bin/env bash
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

# NVSentinel Development Environment Setup
#
# This script installs all required dependencies for NVSentinel development
# on Linux (x86_64/arm64) and macOS (amd64/arm64).
#
# Usage:
#   ./scripts/setup-dev-env.sh                    # Interactive mode
#   ./scripts/setup-dev-env.sh --auto             # Non-interactive mode
#   ./scripts/setup-dev-env.sh --skip-go          # Skip Go installation
#   ./scripts/setup-dev-env.sh --skip-docker      # Skip Docker check
#   ./scripts/setup-dev-env.sh --help             # Show help

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSIONS_FILE="${REPO_ROOT}/.versions.yaml"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
AUTO_MODE=false
SKIP_GO=false
SKIP_DOCKER=false
SKIP_PYTHON=false
SKIP_TOOLS=false

# Detect OS and architecture
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "${ARCH}" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo -e "${RED}❌ Unsupported architecture: ${ARCH}${NC}" && exit 1 ;;
esac

# Helper functions
log_info() {
    echo -e "${BLUE}ℹ️  $*${NC}"
}

log_success() {
    echo -e "${GREEN}✅ $*${NC}"
}

log_warning() {
    echo -e "${YELLOW}⚠️  $*${NC}"
}

log_error() {
    echo -e "${RED}❌ $*${NC}"
}

command_exists() {
    command -v "$1" >/dev/null 2>&1
}

prompt_continue() {
    if [[ "${AUTO_MODE}" == "true" ]]; then
        return 0
    fi
    
    read -p "Continue? [Y/n] " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]] && [[ -n $REPLY ]]; then
        log_warning "Skipped"
        return 1
    fi
    return 0
}

show_help() {
    cat << EOF
NVSentinel Development Environment Setup

Usage: $0 [OPTIONS]

OPTIONS:
    --auto              Non-interactive mode (auto-yes to all prompts)
    --skip-go           Skip Go installation
    --skip-docker       Skip Docker installation/check
    --skip-python       Skip Python/Poetry installation
    --skip-tools        Skip development tools installation
    --help              Show this help message

EXAMPLES:
    # Interactive mode
    $0

    # Automated CI setup
    $0 --auto

    # Install only tools (Go already installed)
    $0 --skip-go --skip-docker

EOF
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --auto)
            AUTO_MODE=true
            shift
            ;;
        --skip-go)
            SKIP_GO=true
            shift
            ;;
        --skip-docker)
            SKIP_DOCKER=true
            shift
            ;;
        --skip-python)
            SKIP_PYTHON=true
            shift
            ;;
        --skip-tools)
            SKIP_TOOLS=true
            shift
            ;;
        --help|-h)
            show_help
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            show_help
            exit 1
            ;;
    esac
done

# Banner
echo ""
echo "╔════════════════════════════════════════════════════════╗"
echo "║   NVSentinel Development Environment Setup            ║"
echo "╚════════════════════════════════════════════════════════╝"
echo ""
log_info "Platform: ${OS}-${ARCH}"
echo ""

# Check if .versions.yaml exists
if [[ ! -f "${VERSIONS_FILE}" ]]; then
    log_error "Version file not found: ${VERSIONS_FILE}"
    log_error "Please run this script from the repository root or ensure .versions.yaml exists"
    exit 1
fi

# Install yq if not present
if ! command_exists yq; then
    log_info "Installing yq (YAML processor)..."
    
    if [[ "${OS}" == "darwin" ]]; then
        if command_exists brew; then
            brew install yq
        else
            log_error "Homebrew not found. Please install Homebrew first: https://brew.sh"
            exit 1
        fi
    elif [[ "${OS}" == "linux" ]]; then
        sudo wget -qO /usr/local/bin/yq https://github.com/mikefarah/yq/releases/latest/download/yq_linux_${ARCH}
        sudo chmod +x /usr/local/bin/yq
    fi
    
    log_success "yq installed"
else
    log_success "yq already installed: $(yq --version)"
fi

# Load versions from .versions.yaml
log_info "Loading versions from .versions.yaml..."
cd "${REPO_ROOT}"

GO_VERSION=$(yq '.languages.go' .versions.yaml)
PYTHON_VERSION=$(yq '.languages.python' .versions.yaml)
POETRY_VERSION=$(yq '.build_tools.poetry' .versions.yaml)
GOLANGCI_LINT_VERSION=$(yq '.go_tools.golangci_lint' .versions.yaml)
PROTOBUF_VERSION=$(yq '.protobuf.protobuf' .versions.yaml)
PROTOC_GEN_GO_VERSION=$(yq '.protobuf.protoc_gen_go' .versions.yaml)
PROTOC_GEN_GO_GRPC_VERSION=$(yq '.protobuf.protoc_gen_go_grpc' .versions.yaml)
GRPCIO_TOOLS_VERSION=$(yq '.protobuf.grpcio_tools' .versions.yaml)
BLACK_VERSION=$(yq '.linting.black' .versions.yaml)
SHELLCHECK_VERSION=$(yq '.linting.shellcheck' .versions.yaml)

echo ""
log_info "Target Versions:"
echo "  Go:              ${GO_VERSION}"
echo "  Python:          ${PYTHON_VERSION}"
echo "  Poetry:          ${POETRY_VERSION}"
echo "  golangci-lint:   ${GOLANGCI_LINT_VERSION}"
echo "  protobuf:        ${PROTOBUF_VERSION}"
echo "  black:           ${BLACK_VERSION}"
echo "  shellcheck:      ${SHELLCHECK_VERSION}"
echo ""

# ============================================================================
# Go Installation
# ============================================================================
if [[ "${SKIP_GO}" == "false" ]]; then
    echo "════════════════════════════════════════════════════════"
    log_info "Go Installation"
    echo "════════════════════════════════════════════════════════"
    
    if command_exists go; then
        CURRENT_GO=$(go version | grep -o 'go[0-9]\+\.[0-9]\+\.[0-9]\+' | sed 's/go//' || echo "unknown")
        log_info "Current Go version: ${CURRENT_GO}"
        
        if [[ "${CURRENT_GO}" == "${GO_VERSION}"* ]]; then
            log_success "Go ${GO_VERSION} already installed"
        else
            log_warning "Go version mismatch (current: ${CURRENT_GO}, target: ${GO_VERSION})"
            log_info "To install Go ${GO_VERSION}, run: make install-go-ci"
        fi
    else
        log_warning "Go not found"
        log_info "To install Go ${GO_VERSION}, run: make install-go-ci"
    fi
    echo ""
fi

# ============================================================================
# Docker Check
# ============================================================================
if [[ "${SKIP_DOCKER}" == "false" ]]; then
    echo "════════════════════════════════════════════════════════"
    log_info "Docker Check"
    echo "════════════════════════════════════════════════════════"
    
    if command_exists docker; then
        if docker info >/dev/null 2>&1; then
            DOCKER_VERSION=$(docker version --format '{{.Server.Version}}')
            log_success "Docker is installed and running: ${DOCKER_VERSION}"
        else
            log_warning "Docker is installed but not running"
            log_info "Please start Docker Desktop or the Docker daemon"
        fi
    else
        log_warning "Docker not found"
        log_info "Please install Docker: https://docs.docker.com/get-docker/"
    fi
    echo ""
fi

# ============================================================================
# Python/Poetry Installation
# ============================================================================
if [[ "${SKIP_PYTHON}" == "false" ]]; then
    echo "════════════════════════════════════════════════════════"
    log_info "Python & Poetry Installation"
    echo "════════════════════════════════════════════════════════"
    
    # Check Python
    if command_exists python3; then
        PYTHON_INSTALLED=$(python3 --version | grep -o '[0-9]\+\.[0-9]\+' || echo "unknown")
        log_success "Python installed: ${PYTHON_INSTALLED}"
    else
        log_warning "Python3 not found"
        if [[ "${OS}" == "darwin" ]]; then
            log_info "Install with: brew install python@${PYTHON_VERSION}"
        elif [[ "${OS}" == "linux" ]]; then
            log_info "Install with: sudo apt-get install -y python3 python3-pip"
        fi
    fi
    
    # Check/Install Poetry
    if command_exists poetry; then
        POETRY_INSTALLED=$(poetry --version | grep -o '[0-9]\+\.[0-9]\+\.[0-9]\+' || echo "unknown")
        log_success "Poetry installed: ${POETRY_INSTALLED}"
        
        if [[ "${POETRY_INSTALLED}" != "${POETRY_VERSION}"* ]]; then
            log_warning "Poetry version mismatch (current: ${POETRY_INSTALLED}, target: ${POETRY_VERSION})"
            log_info "Consider updating: pip install --upgrade poetry==${POETRY_VERSION}"
        fi
    else
        log_warning "Poetry not found"
        log_info "Installing Poetry ${POETRY_VERSION}..."
        
        if prompt_continue; then
            if [[ "${OS}" == "darwin" ]]; then
                pip3 install poetry==${POETRY_VERSION}
            elif [[ "${OS}" == "linux" ]]; then
                python3 -m pip install --break-system-packages poetry==${POETRY_VERSION} || \
                    python3 -m pip install --user poetry==${POETRY_VERSION}
            fi
            log_success "Poetry installed"
        fi
    fi

    # Check/Install Black
    if command_exists black; then
        BLACK_INSTALLED=$(black --version | grep -o '[0-9]\+\.[0-9]\+\.[0-9]\+' || echo "unknown")
        log_success "Black installed: ${BLACK_INSTALLED}"

        if [[ "${BLACK_INSTALLED}" != "${BLACK_VERSION}"* ]]; then
            log_warning "Black version mismatch (current: ${BLACK_INSTALLED}, target: ${BLACK_VERSION})"
            log_info "Consider updating: pip install --upgrade black==${PBLACK_VERSION}"
        fi
    else
        log_warning "Black not found"
        log_info "Installing Black ${BLACK_VERSION}..."

        if prompt_continue; then
            if [[ "${OS}" == "darwin" ]]; then
                pip3 install black==${BLACK_VERSION}
            elif [[ "${OS}" == "linux" ]]; then
                python3 -m pip install --break-system-packages black==${BLACK_VERSION} || \
                    python3 -m pip install --user black==${BLACK_VERSION}
            fi
            log_success "Black installed"
        fi
    fi
    echo ""
fi

# ============================================================================
# Development Tools Installation
# ============================================================================
if [[ "${SKIP_TOOLS}" == "false" ]]; then
    echo "════════════════════════════════════════════════════════"
    log_info "Development Tools Installation"
    echo "════════════════════════════════════════════════════════"
    
    # Helm
    if command_exists helm; then
        log_success "Helm already installed: $(helm version --short)"
    else
        log_info "Installing Helm..."
        if prompt_continue; then
            curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
            log_success "Helm installed"
        fi
    fi
    
    # kubectl
    if command_exists kubectl; then
        log_success "kubectl already installed: $(kubectl version --client --short 2>/dev/null || kubectl version --client)"
    else
        log_info "Installing kubectl..."
        if prompt_continue; then
            if [[ "${OS}" == "darwin" ]]; then
                brew install kubectl
            elif [[ "${OS}" == "linux" ]]; then
                KUBECTL_VERSION=$(curl -L -s https://dl.k8s.io/release/stable.txt)
                sudo curl -L "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl" -o /usr/local/bin/kubectl
                sudo chmod +x /usr/local/bin/kubectl
            fi
            log_success "kubectl installed"
        fi
    fi
    
    # Protocol Buffers
    if command_exists protoc; then
        log_success "protoc already installed: $(protoc --version)"
    else
        log_info "Installing Protocol Buffers ${PROTOBUF_VERSION}..."
        if prompt_continue; then
            PROTOBUF_VERSION_NUM=${PROTOBUF_VERSION#v}
            
            if [[ "${OS}" == "darwin" ]]; then
                PROTOC_ZIP="protoc-${PROTOBUF_VERSION_NUM}-osx-universal_binary.zip"
            elif [[ "${OS}" == "linux" ]]; then
                PROTOC_ZIP="protoc-${PROTOBUF_VERSION_NUM}-linux-${ARCH}.zip"
            fi
            
            TMP_DIR=$(mktemp -d)
            cd "${TMP_DIR}"
            wget -q "https://github.com/protocolbuffers/protobuf/releases/download/${PROTOBUF_VERSION}/${PROTOC_ZIP}"
            unzip -q "${PROTOC_ZIP}"
            sudo cp bin/protoc /usr/local/bin/
            sudo mkdir -p /usr/local/include
            sudo cp -r include/* /usr/local/include/
            cd - >/dev/null
            rm -rf "${TMP_DIR}"
            
            log_success "protoc installed"
        fi
    fi
    
    # shellcheck
    if command_exists shellcheck; then
        log_success "shellcheck already installed: $(shellcheck --version | head -2 | tail -1)"
    else
        log_info "Installing shellcheck ${SHELLCHECK_VERSION}..."
        if prompt_continue; then
            SHELLCHECK_VERSION_NUM=${SHELLCHECK_VERSION#v}
            
            if [[ "${OS}" == "darwin" ]]; then
                brew install shellcheck
            elif [[ "${OS}" == "linux" ]]; then
                TMP_DIR=$(mktemp -d)
                cd "${TMP_DIR}"
                wget -q "https://github.com/koalaman/shellcheck/releases/download/${SHELLCHECK_VERSION}/shellcheck-${SHELLCHECK_VERSION_NUM}.linux.${ARCH}.tar.xz"
                tar -xJ -f "shellcheck-${SHELLCHECK_VERSION_NUM}.linux.${ARCH}.tar.xz"
                sudo cp "shellcheck-${SHELLCHECK_VERSION_NUM}/shellcheck" /usr/local/bin/
                sudo chmod +x /usr/local/bin/shellcheck
                cd - >/dev/null
                rm -rf "${TMP_DIR}"
            fi
            
            log_success "shellcheck installed"
        fi
    fi
    
    # Tilt
    if command_exists tilt; then
        log_success "Tilt already installed: $(tilt version)"
    else
        log_info "Installing Tilt..."
        if prompt_continue; then
            if [[ "${OS}" == "darwin" ]]; then
                brew install tilt
            elif [[ "${OS}" == "linux" ]]; then
                curl -fsSL https://raw.githubusercontent.com/tilt-dev/tilt/master/scripts/install.sh | bash
            fi
            log_success "Tilt installed"
        fi
    fi
    
    # Kind
    if command_exists kind; then
        log_success "Kind already installed: $(kind version)"
    else
        log_info "Installing Kind..."
        if prompt_continue; then
            if [[ "${OS}" == "darwin" ]]; then
                brew install kind
            elif [[ "${OS}" == "linux" ]]; then
                go install sigs.k8s.io/kind@v0.30.0
                sudo cp "$(go env GOPATH)/bin/kind" /usr/local/bin/
            fi
            log_success "Kind installed"
        fi
    fi
    
    # ctlptl
    if command_exists ctlptl; then
        log_success "ctlptl already installed: $(ctlptl version)"
    else
        log_info "Installing ctlptl..."
        if prompt_continue; then
            if [[ "${OS}" == "darwin" ]]; then
                brew install tilt-dev/tap/ctlptl
            elif [[ "${OS}" == "linux" ]]; then
                go install github.com/tilt-dev/ctlptl/cmd/ctlptl@latest
                sudo cp "$(go env GOPATH)/bin/ctlptl" /usr/local/bin/
            fi
            log_success "ctlptl installed"
        fi
    fi
    
    echo ""
fi

# ============================================================================
# Go Development Tools
# ============================================================================
if [[ "${SKIP_TOOLS}" == "false" ]] && command_exists go; then
    echo "════════════════════════════════════════════════════════"
    log_info "Go Development Tools"
    echo "════════════════════════════════════════════════════════"
    
    log_info "Installing Go development tools via Makefile..."
    log_info "This will install: golangci-lint, gotestsum, gocover-cobertura, and more"
    
    if prompt_continue; then
        cd "${REPO_ROOT}"
        make install-lint-tools
        log_success "Go development tools installed"
    fi
    
    echo ""
fi

# ============================================================================
# Python gRPC Tools
# ============================================================================
if [[ "${SKIP_PYTHON}" == "false" ]] && command_exists python3; then
    echo "════════════════════════════════════════════════════════"
    log_info "Python gRPC Tools"
    echo "════════════════════════════════════════════════════════"
    
    log_info "Installing Python gRPC tools (grpcio, grpcio-tools)..."
    
    if prompt_continue; then
        if [[ "${OS}" == "darwin" ]]; then
            # macOS with Homebrew Python requires --break-system-packages or --user
            pip3 install --break-system-packages "grpcio==${GRPCIO_TOOLS_VERSION}" "grpcio-tools==${GRPCIO_TOOLS_VERSION}" 2>/dev/null || \
                pip3 install --user "grpcio==${GRPCIO_TOOLS_VERSION}" "grpcio-tools==${GRPCIO_TOOLS_VERSION}"
        elif [[ "${OS}" == "linux" ]]; then
            python3 -m pip install --break-system-packages \
                "grpcio==${GRPCIO_TOOLS_VERSION}" "grpcio-tools==${GRPCIO_TOOLS_VERSION}" || \
                python3 -m pip install --user \
                "grpcio==${GRPCIO_TOOLS_VERSION}" "grpcio-tools==${GRPCIO_TOOLS_VERSION}"
        fi
        log_success "Python gRPC tools installed"
    fi
    
    echo ""
fi

# ============================================================================
# Summary
# ============================================================================
echo "════════════════════════════════════════════════════════"
log_success "Setup Complete!"
echo "════════════════════════════════════════════════════════"
echo ""
log_info "Installed Tools Summary:"
echo ""

# Check all tools
TOOLS=(
    "yq:yq --version"
    "go:go version"
    "docker:docker version --format {{.Server.Version}}"
    "python3:python3 --version"
    "poetry:poetry --version"
    "helm:helm version --short"
    "kubectl:kubectl version --client --short 2>/dev/null || kubectl version --client"
    "protoc:protoc --version"
    "shellcheck:shellcheck --version | head -2 | tail -1"
    "tilt:tilt version"
    "kind:kind version"
    "ctlptl:ctlptl version"
    "golangci-lint:golangci-lint version 2>/dev/null | head -1"
    "gotestsum:echo installed"
    "addlicense:echo installed"
)

for tool_spec in "${TOOLS[@]}"; do
    tool_name="${tool_spec%%:*}"
    tool_cmd="${tool_spec#*:}"
    
    printf "  %-20s " "${tool_name}:"
    if command_exists "${tool_name}"; then
        version=$(eval "${tool_cmd}" 2>/dev/null || echo "installed")
        echo -e "${GREEN}✓${NC} ${version}"
    else
        echo -e "${YELLOW}✗ not installed${NC}"
    fi
done

echo ""
log_info "Next Steps:"
echo "  1. Verify all tools: make show-versions"
echo "  2. Run tests: make lint-test-all"
echo "  3. Start development: make dev-env"
echo ""
log_info "For more information, see DEVELOPMENT.md"
echo ""
