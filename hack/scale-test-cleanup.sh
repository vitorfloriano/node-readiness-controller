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
PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="${PROJECT_DIR}/hack/tools/bin"
KUBECONFIG_PATH="${PROJECT_DIR}/.kubeconfig-scale"
KWOKCTL="${BIN_DIR}/kwokctl"

# Ensure tools bin directory is in PATH
export PATH="${BIN_DIR}:${PATH}"

if [[ ! -f "${KWOKCTL}" ]]; then
    echo "kwokctl not found locally at ${KWOKCTL}. Nothing to clean up."
    exit 0
fi

if "${KWOKCTL}" get clusters | grep -q "${CLUSTER_NAME}"; then
    echo "Deleting kwokctl cluster '${CLUSTER_NAME}'..."
    "${KWOKCTL}" delete cluster --name "${CLUSTER_NAME}"
fi

rm -f "${KUBECONFIG_PATH}"
echo "Cluster deleted and cleanup complete."
