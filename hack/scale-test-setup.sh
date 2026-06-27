#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
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

set -o errexit
set -o nounset
set -o pipefail

CLUSTER_NAME="nrr-scale-test"
KWOK_VERSION="${KWOK_VERSION:-v0.8.0}"
PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="${PROJECT_DIR}/hack/tools/bin"
KUBECONFIG_PATH="${PROJECT_DIR}/.kubeconfig-scale"
KWOKCTL="${BIN_DIR}/kwokctl"
KWOK="${BIN_DIR}/kwok"

# Ensure tools bin directory exists and is in PATH
mkdir -p "${BIN_DIR}"
export PATH="${BIN_DIR}:${PATH}"

# Detect OS and Architecture
OS="$(uname | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
if [[ "${ARCH}" == "x86_64" ]]; then
    ARCH="amd64"
elif [[ "${ARCH}" == "aarch64" ]]; then
    ARCH="arm64"
fi

# Function to download kwok/kwokctl locally if not present
ensure_kwok_tools() {
    if [[ ! -f "${KWOKCTL}" ]]; then
        echo "kwokctl not found locally at ${KWOKCTL}. Downloading version ${KWOK_VERSION}..."
        curl -sLo "${KWOKCTL}" "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwokctl-${OS}-${ARCH}"
        chmod +x "${KWOKCTL}"
    fi

    if [[ ! -f "${KWOK}" ]]; then
        echo "kwok not found locally at ${KWOK}. Downloading version ${KWOK_VERSION}..."
        curl -sLo "${KWOK}" "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwok-${OS}-${ARCH}"
        chmod +x "${KWOK}"
    fi
}

# Main Setup Execution
ensure_kwok_tools

# Check if cluster already exists
if ! "${KWOKCTL}" get clusters | grep -q "${CLUSTER_NAME}"; then
    echo "Creating kwokctl cluster '${CLUSTER_NAME}'..."
    "${KWOKCTL}" create cluster \
        --name "${CLUSTER_NAME}" \
        --runtime binary \
        --node-lease-duration-seconds 400 \
        --disable-qps-limits \
        --enable-crds Stage \
        --prometheus-port 9090
else
    echo "Cluster '${CLUSTER_NAME}' already exists. Re-using existing cluster."
fi

# Export kubeconfig
"${KWOKCTL}" get kubeconfig --name "${CLUSTER_NAME}" > "${KUBECONFIG_PATH}"

# Install CRDs
echo "Installing NodeReadinessRule CRD..."
KUBECONFIG="${KUBECONFIG_PATH}" kubectl apply -f "${PROJECT_DIR}/config/crd/bases/"

# Verify context and connectivity
echo "Checking cluster connection..."
KUBECONFIG="${KUBECONFIG_PATH}" kubectl cluster-info

echo "===================================================="
echo " kwokctl cluster '${CLUSTER_NAME}' is ready!"
echo " Kubeconfig exported to: ${KUBECONFIG_PATH}"
echo "===================================================="
