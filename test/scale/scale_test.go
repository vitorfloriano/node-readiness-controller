package scale_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/node-readiness-controller/test/utils"
)

type Metadata struct {
	Type string `json:"type"`
	Help string `json:"help"`
}

type MetadataResponse struct {
	Status string                `json:"status"`
	Data   map[string][]Metadata `json:"data"`
}

type QueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
		} `json:"result"`
	} `json:"data"`
}

var reports []string

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

var _ = AfterSuite(func() {
	// 1. Determine artifacts directory
	projectRootDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	artifactsDir := os.Getenv("ARTIFACTS")
	if artifactsDir == "" {
		artifactsDir = filepath.Join(projectRootDir, "test", "scale", "artifacts")
	}

	err = os.MkdirAll(artifactsDir, 0755)
	Expect(err).NotTo(HaveOccurred())

	// 2. Generate and write Markdown Performance Report
	var sb strings.Builder
	sb.WriteString("# Node Readiness Controller Scale Test Performance Report\n\n")
	for _, r := range reports {
		sb.WriteString(r)
		sb.WriteString("\n---\n\n")
	}
	reportContent := sb.String()

	reportPath := filepath.Join(artifactsDir, "performance_report.md")
	err = os.WriteFile(reportPath, []byte(reportContent), 0644)
	Expect(err).NotTo(HaveOccurred())

	// 3. Export Controller log file
	srcLogPath := filepath.Join(projectRootDir, "controller.log")
	if _, err := os.Stat(srcLogPath); err == nil {
		destLogPath := filepath.Join(artifactsDir, "controller.log")
		_ = copyFile(srcLogPath, destLogPath)
	}

	// 4. Stop the kwok cluster to flush TSDB safely
	By("Stopping the kwokctl cluster to flush TSDB...")
	stopCmd := exec.Command(kwokctlBinaryPath, "stop", "cluster", "--name", "kwok")
	_, _ = utils.Run(stopCmd)

	// Wait 2 seconds for Prometheus to shutdown completely
	time.Sleep(2 * time.Second)

	// 5. Tar the Prometheus TSDB directory
	homeDir, err := os.UserHomeDir()
	Expect(err).NotTo(HaveOccurred())
	prometheusDataDir := filepath.Join(homeDir, ".kwok", "clusters", "kwok", "data")

	if _, err := os.Stat(prometheusDataDir); err == nil {
		tarPath := filepath.Join(artifactsDir, "prometheus_tsdb.tar.gz")
		tarCmd := exec.Command("tar", "-czf", tarPath, "-C", prometheusDataDir, ".")
		_, err = utils.Run(tarCmd)
		Expect(err).NotTo(HaveOccurred())
	}
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

	// Sleep 7 seconds to allow Prometheus to scrape final metrics before querying
	By("Waiting for Prometheus scrape interval to capture final metrics...")
	time.Sleep(7 * time.Second)

	// Query and report all metrics dynamically
	phaseName := fmt.Sprintf("%d Nodes Phase", targetReplicas)
	reportStr, err := collectAndReportMetrics(ctx, phaseName)
	Expect(err).NotTo(HaveOccurred())

	reports = append(reports, reportStr)
}

func queryPrometheusInstant(query string) (string, error) {
	urlStr := fmt.Sprintf("http://127.0.0.1:9090/api/v1/query?query=%s", url.QueryEscape(query))
	resp, err := http.Get(urlStr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var promResp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []interface{} `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		return "", err
	}

	if promResp.Status != "success" || len(promResp.Data.Result) == 0 || len(promResp.Data.Result[0].Value) < 2 {
		return "0", nil
	}

	valStr, ok := promResp.Data.Result[0].Value[1].(string)
	if !ok {
		return "0", nil
	}
	return valStr, nil
}

func collectAndReportMetrics(ctx context.Context, phaseName string) (string, error) {
	// 1. Fetch metadata
	metaResp, err := http.Get("http://127.0.0.1:9090/api/v1/metadata")
	if err != nil {
		return "", fmt.Errorf("failed to fetch metadata: %w", err)
	}
	defer metaResp.Body.Close()

	var metadata MetadataResponse
	if err := json.NewDecoder(metaResp.Body).Decode(&metadata); err != nil {
		return "", fmt.Errorf("failed to decode metadata: %w", err)
	}

	// 2. Fetch active series
	seriesResp, err := http.Get("http://127.0.0.1:9090/api/v1/query?query={job=\"node-readiness-controller\"}")
	if err != nil {
		return "", fmt.Errorf("failed to fetch series: %w", err)
	}
	defer seriesResp.Body.Close()

	var series QueryResponse
	if err := json.NewDecoder(seriesResp.Body).Decode(&series); err != nil {
		return "", fmt.Errorf("failed to decode series: %w", err)
	}

	// 3. Deduplicate and categorize metrics
	histograms := make(map[string]bool)
	counters := make(map[string]bool)
	gauges := make(map[string]bool)

	for _, res := range series.Data.Result {
		name := res.Metric["__name__"]
		if name == "" {
			continue
		}

		// Check if it's a histogram suffix
		baseName := name
		isHist := false
		if strings.HasSuffix(name, "_bucket") {
			baseName = strings.TrimSuffix(name, "_bucket")
			isHist = true
		} else if strings.HasSuffix(name, "_sum") {
			baseName = strings.TrimSuffix(name, "_sum")
			isHist = true
		} else if strings.HasSuffix(name, "_count") {
			baseName = strings.TrimSuffix(name, "_count")
			// Double check in metadata if this baseName is actually a histogram
			if meta, ok := metadata.Data[baseName]; ok && len(meta) > 0 && meta[0].Type == "histogram" {
				isHist = true
			}
		}

		if isHist {
			histograms[baseName] = true
			continue
		}

		// Look up type in metadata
		if meta, ok := metadata.Data[name]; ok && len(meta) > 0 {
			switch meta[0].Type {
			case "counter":
				counters[name] = true
			case "gauge":
				gauges[name] = true
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Performance Report: %s\n\n", phaseName))

	// Format Histograms
	if len(histograms) > 0 {
		sb.WriteString("### Histograms (Latency & Durations)\n")
		for name := range histograms {
			p50Query := fmt.Sprintf("histogram_quantile(0.50, sum(rate(%s_bucket[2m])) by (le))", name)
			p90Query := fmt.Sprintf("histogram_quantile(0.90, sum(rate(%s_bucket[2m])) by (le))", name)
			p99Query := fmt.Sprintf("histogram_quantile(0.99, sum(rate(%s_bucket[2m])) by (le))", name)

			p50, _ := queryPrometheusInstant(p50Query)
			p90, _ := queryPrometheusInstant(p90Query)
			p99, _ := queryPrometheusInstant(p99Query)

			sb.WriteString(fmt.Sprintf("*   `%s`:\n", name))
			sb.WriteString(fmt.Sprintf("    *   **P50 (Median)**: %s s\n", p50))
			sb.WriteString(fmt.Sprintf("    *   **P90**: %s s\n", p90))
			sb.WriteString(fmt.Sprintf("    *   **P99 (Tail)**: %s s\n", p99))
		}
		sb.WriteString("\n")
	}

	// Format Counters
	if len(counters) > 0 {
		sb.WriteString("### Counters (Totals & Accumulations)\n")
		for name := range counters {
			increaseQuery := fmt.Sprintf("sum(increase(%s[2m]))", name)
			inc, _ := queryPrometheusInstant(increaseQuery)
			sb.WriteString(fmt.Sprintf("*   `%s` (Total Increase in 2m): **%s**\n", name, inc))
		}
		sb.WriteString("\n")
	}

	// Format Gauges
	if len(gauges) > 0 {
		sb.WriteString("### Gauges (State & Memory/CPU Quantities)\n")
		for name := range gauges {
			maxQuery := fmt.Sprintf("max_over_time(%s[5m])", name)
			avgQuery := fmt.Sprintf("avg_over_time(%s[5m])", name)

			maxVal, _ := queryPrometheusInstant(maxQuery)
			avgVal, _ := queryPrometheusInstant(avgQuery)

			sb.WriteString(fmt.Sprintf("*   `%s`:\n", name))
			sb.WriteString(fmt.Sprintf("    *   **Max Peak**: %s\n", maxVal))
			sb.WriteString(fmt.Sprintf("    *   **Average**: %s\n", avgVal))
		}
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
