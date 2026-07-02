package scale_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
)

var (
	kwokctlBinaryPath string
	kubeconfigPath    string
)

var _ = BeforeSuite(func() {

	projectRootDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())
	toolsBinDir := filepath.Join(projectRootDir, "hack", "tools", "bin")
	kwokctlBinaryPath = ensureKwokctl(kwokctlVersion, toolsBinDir)

	createCmd := exec.Command(kwokctlBinaryPath,
		"create", "cluster",
		"--runtime", "binary",
		"--prometheus-port", "9090")

	createOuput, err := utils.Run(createCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create kwok cluster:\n%s", createOuput)

	crdConfigPath := filepath.Join(projectRootDir, "config", "crd")
	crdCmd := exec.Command("kubectl", "apply", "-k", crdConfigPath)
	crdOutput, err := utils.Run(crdCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply NodeReadinessRule CRD via Kustomize:\n%s", crdOutput)

	controllerBinName := "node-readiness-controller"
	if runtime.GOOS == "windows" {
		controllerBinName += ".exe"
	}
	controllerBinPath := filepath.Join(toolsBinDir, controllerBinName)
	controllerMainPath := filepath.Join(".", "cmd", "main.go")

	buildCmd := exec.Command("go", "build", "-o", controllerBinPath, controllerMainPath)
	buildOutput, err := utils.Run(buildCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to compile controller manager:\n%s", buildOutput)

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
