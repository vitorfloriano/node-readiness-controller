//go:build scale

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

package scale_test

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/node-readiness-controller/test/utils"
)

func TestScale(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Node Readiness Controller Scale Performance Suite")
}

const (
	kwokctlVersion = "v0.8.0"
	nodeCount      = 1000
)

var (
	kwokctlBinaryPath        string
	controllerBinPath        string
)

//go:embed testdata/security-agent-rule.yaml
var securityAgentRuleManifest string

var _ = BeforeSuite(func() {

	By("Ensuring kwokctl binary is present")
	projectRootDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())
	toolsBinDir := filepath.Join(projectRootDir, "hack", "tools", "bin")
	kwokctlBinaryPath = ensureKwokctl(kwokctlVersion, toolsBinDir)

	By("Cleaning up any existing simulated cluster and stale controller processes")
	// Clean up any existing cluster first to ensure we start fresh
	_ = exec.Command(kwokctlBinaryPath, "delete", "cluster").Run()

	// Clean up any stale controller processes running on the host
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/IM", "node-readiness-controller.exe", "/F").Run()
	} else {
		_ = exec.Command("pkill", "-f", "node-readiness-controller").Run()
	}

	By("Creating the simulated KWOK cluster")
	createArgs := []string{
		"create", "cluster",
		"--runtime", "binary",
		"--prometheus-port", "9090",
		"--enable-crds", "Stage",
	}
	if os.Getenv("DISABLE_QPS_LIMITS") == "true" {
		createArgs = append(createArgs, "--disable-qps-limits")
	}
	if leaseSecs := os.Getenv("NODE_LEASE_DURATION_SECONDS"); leaseSecs != "" {
		createArgs = append(createArgs, "--node-lease-duration-seconds", leaseSecs)
	}

	createCmd := exec.Command(kwokctlBinaryPath, createArgs...) // #nosec G204
	createOuput, err := utils.Run(createCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create kwok cluster:\n%s", createOuput)

	homeDir, err := os.UserHomeDir()
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve user home directory")

	kwokKubeconfig := filepath.Join(homeDir, ".kwok", "clusters", "kwok", "kubeconfig.yaml")
	_ = os.Setenv("KUBECONFIG", kwokKubeconfig)

	By("Applying NodeReadinessRule CRD manifests")
	crdConfigPath := filepath.Join(projectRootDir, "config", "crd")
	crdCmd := exec.Command("kubectl", "apply", "-k", crdConfigPath) // #nosec G204
	crdOutput, err := utils.Run(crdCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply NodeReadinessRule CRD via Kustomize:\n%s", crdOutput)

	By("Compiling node-readiness-controller manager binary")
	controllerBinName := "node-readiness-controller"
	if runtime.GOOS == "windows" {
		controllerBinName += ".exe"
	}
	controllerBinPath = filepath.Join(toolsBinDir, controllerBinName)
	controllerMainPath := filepath.Join(".", "cmd", "main.go")

	buildCmd := exec.Command("go", "build", "-o", controllerBinPath, controllerMainPath) // #nosec G204
	buildOutput, err := utils.Run(buildCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to compile controller manager:\n%s", buildOutput)

	By("Configuring controller scraper job in Prometheus config")
	prometheusConfigPath := filepath.Join(homeDir, ".kwok", "clusters", "kwok", "prometheus.yaml")

	prometheusConfigBytes, err := os.ReadFile(prometheusConfigPath) // #nosec G304
	Expect(err).NotTo(HaveOccurred())

	newConfig := string(prometheusConfigBytes)
	modified := false
	if !strings.Contains(newConfig, "node-readiness-controller") {
		extraJobYAML := `- job_name: node-readiness-controller
  scrape_interval: 1s
  metrics_path: /metrics
  scheme: http
  static_configs:
  - targets:
    - 127.0.0.1:8080
`
		newConfig += extraJobYAML
		modified = true
	}

	if modified {
		err = os.WriteFile(prometheusConfigPath, []byte(newConfig), 0600)
		Expect(err).NotTo(HaveOccurred())

		_ = exec.Command("pkill", "-SIGHUP", "prometheus").Run()
	}

	By("Waiting for Prometheus endpoint to be ready")
	// Verify Prometheus readiness explicitly before proceeding (Item 6)
	Eventually(func(g Gomega) {
		resp, err := http.Get("http://127.0.0.1:9090/-/ready")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
	}, "30s", "1s").Should(Succeed(), "Prometheus is not ready")
})

func ensureKwokctl(version string, targetDir string) string {
	goOS := runtime.GOOS
	goArch := runtime.GOARCH

	binaryName := "kwokctl"
	if goOS == "windows" {
		binaryName += ".exe"
	}
	localBinaryPath := filepath.Join(targetDir, binaryName)

	if _, err := os.Stat(localBinaryPath); err == nil {
		return localBinaryPath
	}

	err := os.MkdirAll(targetDir, 0750)
	Expect(err).NotTo(HaveOccurred(), "Failed to create tools directory structure")
	downloadURL := fmt.Sprintf(
		"https://github.com/kubernetes-sigs/kwok/releases/download/%s/kwokctl-%s-%s",
		version, goOS, goArch,
	)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, downloadURL, nil)
	Expect(err).NotTo(HaveOccurred(), "Failed to create download request")
	resp, err := http.DefaultClient.Do(req) // #nosec G107
	Expect(err).NotTo(HaveOccurred(), "Failed to initiate kwokctl binary download")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		Fail(fmt.Sprintf("Failed to download kwokctl from URL %s: Status %s", downloadURL, resp.Status))
	}

	out, err := os.OpenFile(localBinaryPath, os.O_CREATE|os.O_WRONLY, 0700) // #nosec G304 G302
	Expect(err).NotTo(HaveOccurred(), "Failed to create local binary destination file")
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, resp.Body)
	Expect(err).NotTo(HaveOccurred(), "Failed to write binary content to disk target")

	return localBinaryPath
}
