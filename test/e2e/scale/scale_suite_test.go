//go:build scale
// +build scale

/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scale

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

)

var (
	binDir               string
	controllerBinaryPath string
	kubeconfigPath       string
)

func TestScale(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Scale Suite")
}

var _ = BeforeSuite(func() {
	// Locate project dir and set paths
	wd, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred(), "Failed to get working directory")
	projectDir := filepath.Clean(filepath.Join(wd, "..", "..", ".."))

	binDir = filepath.Join(projectDir, "bin")
	err = os.MkdirAll(binDir, 0755)
	Expect(err).NotTo(HaveOccurred(), "Failed to create bin directory")

	controllerBinaryPath = filepath.Join(binDir, "node-readiness-controller-scale")
	kubeconfigPath = filepath.Join(projectDir, ".kubeconfig-scale")

	// Compile the controller binary once
	By("compiling the controller manager binary")
	cmd := exec.Command("go", "build", "-o", controllerBinaryPath, "./cmd/main.go")
	cmd.Dir = projectDir
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to compile controller manager: %s", string(output)))

	// Ensure the kubeconfig for the scale test cluster exists
	_, err = os.Stat(kubeconfigPath)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Kubeconfig not found at %s. Please run 'make setup-scale-test-env' first.", kubeconfigPath))
})
