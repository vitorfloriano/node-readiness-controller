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
WORKSPACE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUBECONFIG_PATH="${WORKSPACE_DIR}/.kubeconfig-scale"

export KWOKCTL="${KWOKCTL:-kwokctl}"
export KUBECTL="${KUBECTL:-kubectl}"

function check_prereqs() {
    # If kwokctl is not in PATH, check if it's in our local bin/
    if ! command -v "$KWOKCTL" &> /dev/null && [ -f "${WORKSPACE_DIR}/bin/kwokctl" ]; then
        export PATH="${WORKSPACE_DIR}/bin:${PATH}"
        export KWOKCTL="${WORKSPACE_DIR}/bin/kwokctl"
    fi

    if ! command -v "$KWOKCTL" &> /dev/null; then
        echo "kwokctl not found. Attempting to download kwokctl..."
        mkdir -p "${WORKSPACE_DIR}/bin"
        local os
        local arch
        os=$(go env GOOS)
        arch=$(go env GOARCH)
        local kwok_repo="kubernetes-sigs/kwok"
        local kwok_latest_release
        kwok_latest_release=$(curl -sf "https://api.github.com/repos/${kwok_repo}/releases/latest" | jq -r '.tag_name')
        if [ -z "$kwok_latest_release" ] || [ "$kwok_latest_release" = "null" ]; then
            kwok_latest_release="v0.8.0" # Fallback to v0.8.0 if API rate limited
        fi
        local download_url="https://github.com/${kwok_repo}/releases/download/${kwok_latest_release}/kwokctl-${os}-${arch}"
        echo "Downloading kwokctl ${kwok_latest_release} from: ${download_url}"
        if curl -Lo "${WORKSPACE_DIR}/bin/kwokctl" "${download_url}"; then
            chmod +x "${WORKSPACE_DIR}/bin/kwokctl"
            export PATH="${WORKSPACE_DIR}/bin:${PATH}"
            export KWOKCTL="${WORKSPACE_DIR}/bin/kwokctl"
            echo "kwokctl successfully installed to ${WORKSPACE_DIR}/bin/kwokctl"
        else
            echo "Error: Failed to download kwokctl." >&2
            exit 1
        fi
    fi

    for cmd in "$KUBECTL" go; do
        if ! command -v "$cmd" &> /dev/null; then
            echo "Error: $cmd is required but not installed." >&2
            exit 1
        fi
    done
}

function start_cluster() {
    check_prereqs
    echo "Creating kwokctl cluster '${CLUSTER_NAME}'..."
    # Ensure any old config is removed
    rm -f "${KUBECONFIG_PATH}"
    touch "${KUBECONFIG_PATH}"

    # If the cluster already exists, reuse it. Otherwise, create it.
    if "$KWOKCTL" get clusters | grep -q "^${CLUSTER_NAME}$"; then
        echo "Cluster '${CLUSTER_NAME}' already exists. Re-using existing cluster."
    else
        # Create the cluster using binary runtime and disable QPS limits
        "$KWOKCTL" create cluster \
            --name "${CLUSTER_NAME}" \
            --runtime binary \
            --node-lease-duration-seconds 400 \
            --disable-qps-limits \
            --enable-crds Stage
    fi

    # Dynamically retrieve the cluster kubeconfig
    "$KWOKCTL" get kubeconfig --name "${CLUSTER_NAME}" > "${KUBECONFIG_PATH}"
    export KUBECONFIG="${KUBECONFIG_PATH}"

    echo "Installing CRDs into the scale test cluster..."
    # Run make install with KUBECONFIG pointing to our temp file
    KUBECONFIG="${KUBECONFIG_PATH}" make install

    echo "===================================================="
    echo " kwokctl cluster '${CLUSTER_NAME}' is ready!"
    echo " Kubeconfig exported to: ${KUBECONFIG_PATH}"
    echo " To interact with the cluster, run:"
    echo "   export KUBECONFIG=${KUBECONFIG_PATH}"
    echo "   kubectl get nodes"
    echo "===================================================="
}

function stop_cluster() {
    echo "Deleting kwokctl cluster '${CLUSTER_NAME}'..."
    "$KWOKCTL" delete cluster --name "${CLUSTER_NAME}" || true
    rm -f "${KUBECONFIG_PATH}"
    echo "Cluster deleted and cleanup complete."
}

action="${1:-start}"
case "$action" in
    start)
        start_cluster
        ;;
    stop)
        stop_cluster
        ;;
    *)
        echo "Usage: $0 {start|stop}" >&2
        exit 1
        ;;
esac
