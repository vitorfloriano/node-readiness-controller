package scale_test

import (
	"context"
	"encoding/json"
	"fmt"

	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

type PhaseStats struct {
	Title string
	Start time.Time
	End   time.Time
}

var reports []string

var _ = Describe("Node Readiness Controller Scale Performance Test", func() {
	var (
		cmd       *exec.Cmd
		clientset *kubernetes.Clientset
		logFile   *os.File
	)

	BeforeEach(func() {
		// Reset the mutable reports slice to prevent carryover state (Item 7)
		reports = nil

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

		// Resolve artifacts directory
		projectRootDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())

		artifactsDir := os.Getenv("ARTIFACTS")
		if artifactsDir == "" {
			artifactsDir = filepath.Join(projectRootDir, "test", "scale", "artifacts")
		}

		err = os.MkdirAll(artifactsDir, 0755)
		Expect(err).NotTo(HaveOccurred())

		// Create log file for the controller directly in the artifacts directory
		var errError error
		logFile, errError = os.Create(filepath.Join(artifactsDir, "controller.log"))
		Expect(errError).NotTo(HaveOccurred(), "Failed to create controller.log")

		// Start the controller manager process with optional PR 287 concurrency tuning flags
		args := []string{
			"--metrics-bind-address=:8080",
			"--metrics-secure=false",
			"--leader-elect=false",
			"--enable-webhook=false",
		}
		if qps := os.Getenv("KUBE_API_QPS"); qps != "" {
			args = append(args, "--kube-api-qps="+qps)
		}
		if burst := os.Getenv("KUBE_API_BURST"); burst != "" {
			args = append(args, "--kube-api-burst="+burst)
		}
		if nodeConc := os.Getenv("NODE_CONCURRENT_RECONCILES"); nodeConc != "" {
			args = append(args, "--node-concurrent-reconciles="+nodeConc)
		}
		if ruleConc := os.Getenv("RULE_CONCURRENT_RECONCILES"); ruleConc != "" {
			args = append(args, "--rule-concurrent-reconciles="+ruleConc)
		}

		cmd = exec.Command(controllerBinPath, args...)
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

		var phases []int

		if nodeCountStr := os.Getenv("NODE_COUNT"); nodeCountStr != "" {
			count, err := strconv.Atoi(nodeCountStr)
			Expect(err).NotTo(HaveOccurred(), "Invalid NODE_COUNT: %s", nodeCountStr)
			phases = []int{count}
		} else {
			size := strings.ToUpper(os.Getenv("SCALE_SIZE"))
			if size == "" {
				size = "EXTRA_SMALL" // default to 50 nodes for a fast local test
			}

			switch size {
			case "XS", "EXTRA_SMALL":
				phases = []int{50}
			case "S", "SMALL":
				phases = []int{50, 100}
			case "M", "MEDIUM":
				phases = []int{50, 100, 500}
			case "L", "LARGE":
				phases = []int{50, 100, 500, 1000}
			default:
				Fail(fmt.Sprintf("Invalid SCALE_SIZE '%s'. Must be one of: EXTRA_SMALL/XS, SMALL/S, MEDIUM/M, LARGE/L", size))
			}
		}

		By(fmt.Sprintf("Running scale test with phases: %v", phases))
		var completedPhases []PhaseStats
		for _, count := range phases {
			completedPhases = append(completedPhases, runScalePhase(ctx, clientset, count)...)
		}

		for _, phase := range completedPhases {
			report, err := collectAndReportMetricsForWindow(ctx, phase.Title, phase.Start, phase.End)
			Expect(err).NotTo(HaveOccurred())
			reports = append(reports, report)
		}
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



	// 4. Stop the kwok cluster to flush TSDB safely
	By("Stopping the kwokctl cluster to flush TSDB...")
	stopCmd := exec.Command(kwokctlBinaryPath, "stop", "cluster", "--name", "kwok")
	stopOutput, err := utils.Run(stopCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to stop kwok cluster:\n%s", stopOutput)

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

func runScalePhase(ctx context.Context, clientset *kubernetes.Clientset, targetReplicas int) []PhaseStats {
	By(fmt.Sprintf("Scaling nodes to %d", targetReplicas))

	projectRootDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	// 1. Scale using kwokctl
	scaleCmd := exec.Command(kwokctlBinaryPath, "scale", "node",
		"--replicas", strconv.Itoa(targetReplicas),
		"--name", "kwok")
	scaleOutput, err := utils.Run(scaleCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to scale nodes: %s", scaleOutput)

	// Verify all newly scaled nodes are tainted initially
	By("Verifying all newly scaled nodes are tainted initially")
	Eventually(func(g Gomega) int {
		count, err := countTaintedNodes(ctx, clientset)
		g.Expect(err).NotTo(HaveOccurred())
		return count
	}, "15m", "1s").Should(Equal(targetReplicas), "Tainted nodes count did not reach target replicas")

	var phases []PhaseStats

	// ----------------------------------------------------
	// 2. Untaint (Removal) Phase
	// ----------------------------------------------------
	By("Applying calico-readiness-stage-true to remove taints")
	removeStart := time.Now()

	trueStagePath := filepath.Join(projectRootDir, "test", "scale", "testdata", "cni-readiness-stage-true.yaml")
	applyTrueCmd := exec.Command("kubectl", "apply", "-f", trueStagePath)
	trueOutput, err := utils.Run(applyTrueCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply true stage: %s", trueOutput)

	Eventually(func(g Gomega) int {
		count, err := countTaintedNodes(ctx, clientset)
		g.Expect(err).NotTo(HaveOccurred())
		return count
	}, "15m", "1s").Should(Equal(0), "Tainted nodes count did not drop to 0")

	removeEnd := time.Now()
	removeDuration := removeEnd.Sub(removeStart)

	// Clean up the True stage
	deleteTrueCmd := exec.Command("kubectl", "delete", "stage", "calico-readiness-stage-true", "--ignore-not-found")
	_, err = utils.Run(deleteTrueCmd)
	Expect(err).NotTo(HaveOccurred())

	phases = append(phases, PhaseStats{
		Title: fmt.Sprintf("%d Nodes - Untaint (Removal) Phase [Duration: %s]", targetReplicas, removeDuration.Round(time.Millisecond)),
		Start: removeStart,
		End:   removeEnd,
	})

	// ----------------------------------------------------
	// 3. Retaint (Add) Phase
	// ----------------------------------------------------
	By("Applying calico-readiness-stage-false to add taints again")
	addStart := time.Now()

	falseStagePath := filepath.Join(projectRootDir, "test", "scale", "testdata", "cni-readiness-stage-false.yaml")
	applyFalseCmd := exec.Command("kubectl", "apply", "-f", falseStagePath)
	falseOutput, err := utils.Run(applyFalseCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply false stage: %s", falseOutput)

	Eventually(func(g Gomega) int {
		count, err := countTaintedNodes(ctx, clientset)
		g.Expect(err).NotTo(HaveOccurred())
		return count
	}, "15m", "1s").Should(Equal(targetReplicas), "Tainted nodes count did not reach target replicas after setting false")

	addEnd := time.Now()
	addDuration := addEnd.Sub(addStart)

	// Clean up the False stage
	deleteFalseCmd := exec.Command("kubectl", "delete", "stage", "calico-readiness-stage-false", "--ignore-not-found")
	_, err = utils.Run(deleteFalseCmd)
	Expect(err).NotTo(HaveOccurred())

	phases = append(phases, PhaseStats{
		Title: fmt.Sprintf("%d Nodes - Retaint (Add) Phase [Duration: %s]", targetReplicas, addDuration.Round(time.Millisecond)),
		Start: addStart,
		End:   addEnd,
	})

	return phases
}

func doGetRequest(ctx context.Context, urlStr string) (*http.Response, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func queryPrometheusInstant(ctx context.Context, query string) (string, error) {
	urlStr := fmt.Sprintf("http://127.0.0.1:9090/api/v1/query?query=%s", url.QueryEscape(query))
	resp, err := doGetRequest(ctx, urlStr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

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

	if promResp.Status != "success" {
		return "", fmt.Errorf("prometheus query failed: %s", promResp.Status)
	}

	if len(promResp.Data.Result) == 0 || len(promResp.Data.Result[0].Value) < 2 {
		return "", fmt.Errorf("no data returned")
	}

	valStr, ok := promResp.Data.Result[0].Value[1].(string)
	if !ok {
		return "", fmt.Errorf("invalid value format")
	}
	return valStr, nil
}



func collectAndReportMetricsForWindow(ctx context.Context, phaseTitle string, phaseStart time.Time, phaseEnd time.Time) (string, error) {
	// Set evaluation time to phaseEnd + 5s to ensure the final scrape is included (Item 2)
	evalTime := phaseEnd.Add(5 * time.Second)
	lookbackSecs := int(evalTime.Sub(phaseStart).Seconds())
	if lookbackSecs < 10 {
		lookbackSecs = 10 // Ensure a minimum lookback of 10 seconds to include at least two scrapes
	}
	ts := float64(evalTime.UnixNano()) / 1e9

	// 1. Fetch metadata
	metaResp, err := doGetRequest(ctx, "http://127.0.0.1:9090/api/v1/metadata")
	if err != nil {
		return "", fmt.Errorf("failed to fetch metadata: %w", err)
	}
	defer metaResp.Body.Close()

	var metadata MetadataResponse
	if err := json.NewDecoder(metaResp.Body).Decode(&metadata); err != nil {
		return "", fmt.Errorf("failed to decode metadata: %w", err)
	}

	// 2. Fetch active series
	seriesResp, err := doGetRequest(ctx, "http://127.0.0.1:9090/api/v1/query?query={job=\"node-readiness-controller\"}")
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
	sb.WriteString(fmt.Sprintf("## Performance Report: %s\n\n", phaseTitle))

	// Format Histograms
	if len(histograms) > 0 {
		var sortedHistograms []string
		for name := range histograms {
			sortedHistograms = append(sortedHistograms, name)
		}
		sort.Strings(sortedHistograms)

		sb.WriteString("### Histograms (Latency & Durations)\n")
		for _, name := range sortedHistograms {
			p50Query := fmt.Sprintf("histogram_quantile(0.50, sum(rate(%s_bucket[%ds] @ %.3f)) by (le))", name, lookbackSecs, ts)
			p90Query := fmt.Sprintf("histogram_quantile(0.90, sum(rate(%s_bucket[%ds] @ %.3f)) by (le))", name, lookbackSecs, ts)
			p99Query := fmt.Sprintf("histogram_quantile(0.99, sum(rate(%s_bucket[%ds] @ %.3f)) by (le))", name, lookbackSecs, ts)

			p50, err50 := queryPrometheusInstant(ctx, p50Query)
			p90, err90 := queryPrometheusInstant(ctx, p90Query)
			p99, err99 := queryPrometheusInstant(ctx, p99Query)

			sb.WriteString(fmt.Sprintf("*   `%s`:\n", name))
			if err50 != nil {
				sb.WriteString(fmt.Sprintf("    *   **P50 (Median)**: N/A (Error: %v)\n", err50))
			} else {
				sb.WriteString(fmt.Sprintf("    *   **P50 (Median)**: %s s\n", p50))
			}
			if err90 != nil {
				sb.WriteString(fmt.Sprintf("    *   **P90**: N/A (Error: %v)\n", err90))
			} else {
				sb.WriteString(fmt.Sprintf("    *   **P90**: %s s\n", p90))
			}
			if err99 != nil {
				sb.WriteString(fmt.Sprintf("    *   **P99 (Tail)**: N/A (Error: %v)\n", err99))
			} else {
				sb.WriteString(fmt.Sprintf("    *   **P99 (Tail)**: %s s\n", p99))
			}
		}
		sb.WriteString("\n")
	}

	// Format Counters
	if len(counters) > 0 {
		var sortedCounters []string
		for name := range counters {
			sortedCounters = append(sortedCounters, name)
		}
		sort.Strings(sortedCounters)

		sb.WriteString("### Counters (Totals & Accumulations)\n")
		for _, name := range sortedCounters {
			increaseQuery := fmt.Sprintf("sum(increase(%s[%ds] @ %.3f))", name, lookbackSecs, ts)
			inc, errInc := queryPrometheusInstant(ctx, increaseQuery)
			if errInc != nil {
				sb.WriteString(fmt.Sprintf("*   `%s` (Total Increase in %ds): **N/A (Error: %v)**\n", name, lookbackSecs, errInc))
			} else {
				sb.WriteString(fmt.Sprintf("*   `%s` (Total Increase in %ds): **%s**\n", name, lookbackSecs, inc))
			}
		}
		sb.WriteString("\n")
	}

	// Format Gauges
	if len(gauges) > 0 {
		var sortedGauges []string
		for name := range gauges {
			sortedGauges = append(sortedGauges, name)
		}
		sort.Strings(sortedGauges)

		sb.WriteString("### Gauges (State & Memory/CPU Quantities)\n")
		for _, name := range sortedGauges {
			maxQuery := fmt.Sprintf("max_over_time(%s[%ds] @ %.3f)", name, lookbackSecs, ts)
			avgQuery := fmt.Sprintf("avg_over_time(%s[%ds] @ %.3f)", name, lookbackSecs, ts)

			maxVal, errMax := queryPrometheusInstant(ctx, maxQuery)
			avgVal, errAvg := queryPrometheusInstant(ctx, avgQuery)

			sb.WriteString(fmt.Sprintf("*   `%s`:\n", name))
			if errMax != nil {
				sb.WriteString(fmt.Sprintf("    *   **Max Peak**: N/A (Error: %v)\n", errMax))
			} else {
				sb.WriteString(fmt.Sprintf("    *   **Max Peak**: %s\n", maxVal))
			}
			if errAvg != nil {
				sb.WriteString(fmt.Sprintf("    *   **Average**: N/A (Error: %v)\n", errAvg))
			} else {
				sb.WriteString(fmt.Sprintf("    *   **Average**: %s\n", avgVal))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String(), nil
}


