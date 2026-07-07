package scale_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/node-readiness-controller/test/utils"
)

var _ = Describe("Node Readiness Controller Scale Performance Test", func() {
	var (
		cmd       *exec.Cmd
		clientset *kubernetes.Clientset
		logFile   *os.File
	)

	BeforeEach(func() {
		// Clean up any existing controller process
		_ = exec.Command("pkill", "-f", "node-readiness-controller").Run()

		// Resolve kubeconfig path
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home, err := os.UserHomeDir()
			Expect(err).NotTo(HaveOccurred(), "Failed to get user home directory")
			kubeconfig = filepath.Join(home, ".kube", "config")
		}

		// Set up Kubernetes clientset
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		Expect(err).NotTo(HaveOccurred(), "Failed to build client-go config")
		clientset, err = kubernetes.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred(), "Failed to create clientset")

		// Create log file for the controller
		var errError error
		logFile, errError = os.Create("controller.log")
		Expect(errError).NotTo(HaveOccurred(), "Failed to create controller.log")

		// Start the controller manager process
		cmd = exec.Command(controllerBinPath,
			"--metrics-bind-address=:8080",
			"--metrics-secure=false",
			"--leader-elect=false",
			"--enable-webhook=false",
		)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		cmd.Stdout = logFile
		cmd.Stderr = logFile

		err = cmd.Start()
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller process")

		// Wait for the controller metrics to be ready
		Eventually(func(g Gomega) {
			resp, err := http.Get("http://127.0.0.1:8080/metrics")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
		}, "15s", "500ms").Should(Succeed(), "Controller failed to start or bind to port 8080")
	})

	AfterEach(func() {
		// Kill the controller process
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if logFile != nil {
			logFile.Close()
		}

		// Clean up simulated nodes
		_ = clientset.CoreV1().Nodes().DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{
			LabelSelector: "type=kwok",
		})
	})

	It("should successfully run the scale test phases and evaluate performance", func() {
		ctx := context.Background()

		// Run 50 nodes phase
		runScalePhase(ctx, clientset, 50)
	})
})

func countTaintedNodes(ctx context.Context, clientset *kubernetes.Clientset) (int, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "type=kwok"})
	if err != nil {
		return 0, err
	}

	count := 0
	for _, node := range nodes.Items {
		for _, taint := range node.Spec.Taints {
			if taint.Key == "readiness.k8s.io/NetworkReady" && taint.Value == "pending" {
				count++
				break
			}
		}
	}
	return count, nil
}

func runScalePhase(ctx context.Context, clientset *kubernetes.Clientset, targetReplicas int) {
	By(fmt.Sprintf("Scaling nodes to %d", targetReplicas))

	projectRootDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	// 1. Scale using kwokctl
	scaleCmd := exec.Command(kwokctlBinaryPath, "scale", "node",
		"--replicas", strconv.Itoa(targetReplicas),
		"--name", "kwok")
	scaleOutput, err := utils.Run(scaleCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to scale nodes: %s", scaleOutput)

	// 2. Since nodes join pre-tainted by our KwokctlResource config, check that all targetReplicas nodes are tainted
	By("Verifying all newly scaled nodes are tainted initially")
	Eventually(func(g Gomega) int {
		count, err := countTaintedNodes(ctx, clientset)
		g.Expect(err).NotTo(HaveOccurred())
		return count
	}, "30s", "500ms").Should(Equal(targetReplicas), "Tainted nodes count did not reach target replicas")

	// 3. Apply the True stage to remove taints
	By("Applying calico-readiness-stage-true to remove taints")
	trueStagePath := filepath.Join(projectRootDir, "test", "scale", "testdata", "cni-readiness-stage-true.yaml")
	applyTrueCmd := exec.Command("kubectl", "apply", "-f", trueStagePath)
	trueOutput, err := utils.Run(applyTrueCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply true stage: %s", trueOutput)

	// 4. Verify that taints are removed
	By("Verifying taints are removed from all nodes")
	Eventually(func(g Gomega) int {
		count, err := countTaintedNodes(ctx, clientset)
		g.Expect(err).NotTo(HaveOccurred())
		return count
	}, "30s", "500ms").Should(Equal(0), "Tainted nodes count did not drop to 0")

	// 5. Clean up the True stage
	deleteTrueCmd := exec.Command("kubectl", "delete", "stage", "calico-readiness-stage-true", "--ignore-not-found")
	_, err = utils.Run(deleteTrueCmd)
	Expect(err).NotTo(HaveOccurred())

	// 6. Apply the False stage to add taints again
	By("Applying calico-readiness-stage-false to add taints again")
	falseStagePath := filepath.Join(projectRootDir, "test", "scale", "testdata", "cni-readiness-stage-false.yaml")
	applyFalseCmd := exec.Command("kubectl", "apply", "-f", falseStagePath)
	falseOutput, err := utils.Run(applyFalseCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply false stage: %s", falseOutput)

	// 7. Verify that taints are added back
	By("Verifying taints are added back to all nodes")
	Eventually(func(g Gomega) int {
		count, err := countTaintedNodes(ctx, clientset)
		g.Expect(err).NotTo(HaveOccurred())
		return count
	}, "30s", "500ms").Should(Equal(targetReplicas), "Tainted nodes count did not reach target replicas after setting false")

	// 8. Clean up the False stage so we are ready for the next scale phase
	deleteFalseCmd := exec.Command("kubectl", "delete", "stage", "calico-readiness-stage-false", "--ignore-not-found")
	_, err = utils.Run(deleteFalseCmd)
	Expect(err).NotTo(HaveOccurred())
}
