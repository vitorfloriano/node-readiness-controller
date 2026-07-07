package scale_test

import (
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
	kwokctlBinaryPath string
	controllerBinPath string
)

//go:embed testdata/cni-readiness-rule.yaml
var cniReadinessRuleManifest string

//go:embed testdata/cni-readiness-stage-initial.yaml
var cniReadinessStageInitialManifest string

var _ = BeforeSuite(func() {

	projectRootDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())
	toolsBinDir := filepath.Join(projectRootDir, "hack", "tools", "bin")
	kwokctlBinaryPath = ensureKwokctl(kwokctlVersion, toolsBinDir)

	// Clean up any existing cluster first to ensure we start fresh
	_ = exec.Command(kwokctlBinaryPath, "delete", "cluster").Run()

	kwokConfigPath := filepath.Join(projectRootDir, "test", "scale", "testdata", "kwokctl-config.yaml")
	createArgs := []string{
		"create", "cluster",
		"--runtime", "binary",
		"--prometheus-port", "9090",
		"--enable-crds", "Stage",
		"--config", kwokConfigPath,
	}
	if os.Getenv("DISABLE_QPS_LIMITS") == "true" {
		createArgs = append(createArgs, "--disable-qps-limits")
	}
	if leaseSecs := os.Getenv("NODE_LEASE_DURATION_SECONDS"); leaseSecs != "" {
		createArgs = append(createArgs, "--node-lease-duration-seconds", leaseSecs)
	}

	createCmd := exec.Command(kwokctlBinaryPath, createArgs...)
	createOuput, err := utils.Run(createCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create kwok cluster:\n%s", createOuput)

	homeDir, err := os.UserHomeDir()
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve user home directory")

	kwokKubeconfig := filepath.Join(homeDir, ".kwok", "clusters", "kwok", "kubeconfig.yaml")
	os.Setenv("KUBECONFIG", kwokKubeconfig)

	crdConfigPath := filepath.Join(projectRootDir, "config", "crd")
	crdCmd := exec.Command("kubectl", "apply", "-k", crdConfigPath)
	crdOutput, err := utils.Run(crdCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply NodeReadinessRule CRD via Kustomize:\n%s", crdOutput)

	controllerBinName := "node-readiness-controller"
	if runtime.GOOS == "windows" {
		controllerBinName += ".exe"
	}
	controllerBinPath = filepath.Join(toolsBinDir, controllerBinName)
	controllerMainPath := filepath.Join(".", "cmd", "main.go")

	buildCmd := exec.Command("go", "build", "-o", controllerBinPath, controllerMainPath)
	buildOutput, err := utils.Run(buildCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to compile controller manager:\n%s", buildOutput)

	prometheusConfigPath := filepath.Join(homeDir, ".kwok", "clusters", "kwok", "prometheus.yaml")

	prometheusConfigBytes, err := os.ReadFile(prometheusConfigPath)
	Expect(err).NotTo(HaveOccurred())

	if !strings.Contains(string(prometheusConfigBytes), "node-readiness-controller") {
		extraJobYAML := `- job_name: node-readiness-controller
  scrape_interval: 5s
  metrics_path: /metrics
  scheme: http
  static_configs:
  - targets:
    - 127.0.0.1:8080
`
		f, err := os.OpenFile(prometheusConfigPath, os.O_APPEND|os.O_WRONLY, 0644)
		Expect(err).NotTo(HaveOccurred())
		defer f.Close()

		_, err = f.WriteString(extraJobYAML)
		Expect(err).NotTo(HaveOccurred())

		_ = exec.Command("pkill", "-SIGHUP", "prometheus").Run()
	}

	setupRuleCmd := exec.Command("kubectl", "apply", "-f", "-")
	setupRuleCmd.Stdin = strings.NewReader(cniReadinessRuleManifest)

	setupRuleCmdOutput, err := utils.Run(setupRuleCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply CNI NodeReadinessRule manifest:\n%s", setupRuleCmdOutput)
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

	err := os.MkdirAll(targetDir, 0755)
	Expect(err).NotTo(HaveOccurred(), "Failed to create tools directory structure")

	downloadURL := fmt.Sprintf(
		"https://github.com/kubernetes-sigs/kwok/releases/download/%s/kwokctl-%s-%s",
		version, goOS, goArch,
	)

	resp, err := http.Get(downloadURL)
	Expect(err).NotTo(HaveOccurred(), "Failed to initiate kwokctl binary download")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		Fail(fmt.Sprintf("Failed to download kwokctl from URL %s: Status %s", downloadURL, resp.Status))
	}

	out, err := os.OpenFile(localBinaryPath, os.O_CREATE|os.O_WRONLY, 0755)
	Expect(err).NotTo(HaveOccurred(), "Failed to create local binary destination file")
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	Expect(err).NotTo(HaveOccurred(), "Failed to write binary content to disk target")

	return localBinaryPath
}
