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
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/node-readiness-controller/test/utils"
)

type QueryResult struct {
	PhaseTitle      string            `json:"phase_title"`
	DurationSeconds float64           `json:"duration_seconds"`
	Metrics         map[string]string `json:"metrics"`
}

type PhaseStats struct {
	Title string
	Start time.Time
	End   time.Time
}

var (
	queryResults  []QueryResult
	nodeCountUsed int
)

var _ = Describe("Node Readiness Controller Scale Performance Test", func() {
	var (
		cmd       *exec.Cmd
		clientset *kubernetes.Clientset
		logFile   *os.File
	)

	BeforeEach(func() {
		queryResults = nil
		nodeCountUsed = 1000

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

		err = os.MkdirAll(artifactsDir, 0750)
		Expect(err).NotTo(HaveOccurred())

		// Create log file for the controller directly in the artifacts directory
		var errError error
		logFile, errError = os.Create(filepath.Join(artifactsDir, "controller.log")) // #nosec G304
		Expect(errError).NotTo(HaveOccurred(), "Failed to create controller.log")

		// Start the controller manager process
		By("Starting the node-readiness-controller manager daemon process")
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

		if runtime.GOOS != "windows" {
			cmd = exec.Command("setsid", append([]string{controllerBinPath}, args...)...)
		} else {
			cmd = exec.Command(controllerBinPath, args...)
		}
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		cmd.Stdout = logFile
		cmd.Stderr = logFile

		err = cmd.Start()
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller process")

		// Wait for the controller metrics to be ready
		By("Waiting for the controller metrics endpoint to be responsive")
		Eventually(func(g Gomega) {
			resp, err := http.Get("http://127.0.0.1:8080/metrics")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
		}, "15s", "500ms").Should(Succeed(), "Controller failed to start or bind to port 8080")
	})

	AfterEach(func() {
		if os.Getenv("SKIP_TEARDOWN") == "true" {
			By("Skipping controller and node cleanup because SKIP_TEARDOWN is set to true")
			return
		}

		// Kill the controller process
		By("Terminating the background controller process")
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if logFile != nil {
			_ = logFile.Close()
		}

		// Clean up simulated nodes
		_ = clientset.CoreV1().Nodes().DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{
			LabelSelector: "type=kwok",
		})
	})

	It("should successfully run the scale test phases and evaluate performance", func() {
		ctx := context.Background()

		nodeCount := defaultNodeCount
		if nodeCountStr := os.Getenv("NODE_COUNT"); nodeCountStr != "" {
			var err error
			nodeCount, err = strconv.Atoi(nodeCountStr)
			Expect(err).NotTo(HaveOccurred(), "Invalid NODE_COUNT: %s", nodeCountStr)
		}
		nodeCountUsed = nodeCount

		By(fmt.Sprintf("Scaling nodes to %d", nodeCount))

		projectRootDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())

		// Scale using kwokctl
		scaleCmd := exec.CommandContext(ctx, kwokctlBinaryPath, "scale", "node", // #nosec G204 G702
			"--replicas", strconv.Itoa(nodeCount),
			"--name", "kwok")
		scaleOutput, err := utils.Run(scaleCmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to scale nodes: %s", scaleOutput)

		// Verify all newly scaled nodes are created and start untainted
		By("Verifying all newly scaled nodes are created and start untainted")
		Eventually(func(g Gomega) {
			nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "type=kwok"})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(len(nodes.Items)).To(Equal(nodeCount))
			
			tainted, err := countTaintedNodes(ctx, clientset)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tainted).To(Equal(0))
		}, "15m", "1s").Should(Succeed(), "Nodes failed to scale or start untainted")

		var phases []PhaseStats

		// Tainting Phase
		taintStart := time.Now()

		By("Applying Security Agent NodeReadinessRule resource")
		setupRuleCmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		setupRuleCmd.Stdin = strings.NewReader(securityAgentRuleManifest)
		setupRuleCmdOutput, err := utils.Run(setupRuleCmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply Security Agent NodeReadinessRule manifest:\n%s", setupRuleCmdOutput)

		By("Applying security-agent-stage-false stage rules to simulate unhealthy agent status")
		falseStagePath := filepath.Join(projectRootDir, "test", "scale", "testdata", "security-agent-stage-false.yaml")
		applyFalseCmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", falseStagePath) // #nosec G204
		falseOutput, err := utils.Run(applyFalseCmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply false stage: %s", falseOutput)

		By("Waiting for the controller manager to reconcile and add taints to all nodes")
		Eventually(func(g Gomega) int {
			count, err := countTaintedNodes(ctx, clientset)
			g.Expect(err).NotTo(HaveOccurred())
			By(fmt.Sprintf("Progress: %d/%d nodes successfully tainted", count, nodeCount))
			return count
		}, "15m", "1s").Should(Equal(nodeCount), "Tainted nodes count did not reach target replicas")

		taintEnd := time.Now()
		taintDuration := taintEnd.Sub(taintStart)

		// Clean up the False stage
		deleteFalseCmd := exec.CommandContext(ctx, "kubectl", "delete", "stage", "security-agent-stage-false", "--ignore-not-found") // #nosec G204
		_, err = utils.Run(deleteFalseCmd)
		Expect(err).NotTo(HaveOccurred())

		phases = append(phases, PhaseStats{
			Title: fmt.Sprintf("%d Nodes - Tainting (Add) Phase [Duration: %s]", nodeCount, taintDuration.Round(time.Millisecond)),
			Start: taintStart,
			End:   taintEnd,
		})

		// Untainting / Annotation Phase
		By("Applying security-agent-stage-true stage rules to simulate CNI/agent readiness")
		untaintStart := time.Now()

		trueStagePath := filepath.Join(projectRootDir, "test", "scale", "testdata", "security-agent-stage-true.yaml")
		applyTrueCmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", trueStagePath) // #nosec G204
		trueOutput, err := utils.Run(applyTrueCmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply true stage: %s", trueOutput)

		By("Waiting for the controller manager to reconcile, remove taints, and write bootstrap annotations on all nodes")
		Eventually(func(g Gomega) {
			tainted, err := countTaintedNodes(ctx, clientset)
			g.Expect(err).NotTo(HaveOccurred())
			annotated, err := countAnnotatedNodes(ctx, clientset)
			g.Expect(err).NotTo(HaveOccurred())
			By(fmt.Sprintf("Progress: %d/%d nodes remaining tainted, %d/%d nodes annotated", tainted, nodeCount, annotated, nodeCount))
			g.Expect(tainted).To(Equal(0))
			g.Expect(annotated).To(Equal(nodeCount))
		}, "15m", "1s").Should(Succeed(), "Failed to complete untainting and annotation phase")

		untaintEnd := time.Now()
		untaintDuration := untaintEnd.Sub(untaintStart)

		// Clean up the True stage
		deleteTrueCmd := exec.CommandContext(ctx, "kubectl", "delete", "stage", "security-agent-stage-true", "--ignore-not-found") // #nosec G204
		_, err = utils.Run(deleteTrueCmd)
		Expect(err).NotTo(HaveOccurred())

		phases = append(phases, PhaseStats{
			Title: fmt.Sprintf("%d Nodes - Untainting / Annotation Phase [Duration: %s]", nodeCount, untaintDuration.Round(time.Millisecond)),
			Start: untaintStart,
			End:   untaintEnd,
		})

		for _, phase := range phases {
			reportStruct, err := collectAndReportMetricsForWindow(ctx, phase.Title, phase.Start, phase.End)
			Expect(err).NotTo(HaveOccurred())
			queryResults = append(queryResults, reportStruct)
		}
	})
})

var _ = AfterSuite(func() {
	projectRootDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	artifactsDir := os.Getenv("ARTIFACTS")
	if artifactsDir == "" {
		artifactsDir = filepath.Join(projectRootDir, "test", "scale", "artifacts")
	}

	err = os.MkdirAll(artifactsDir, 0750)
	Expect(err).NotTo(HaveOccurred())

	// Generate and write Markdown Performance Report
	By("Writing Markdown performance report")
	templatePath := filepath.Join(projectRootDir, "test", "scale", "testdata", "scalability_report.md.tmpl")
	tmpl, err := template.ParseFiles(templatePath)
	Expect(err).NotTo(HaveOccurred())

	reportData := struct {
		NodeCount int
		Mode      string
		Phases    []QueryResult
	}{
		NodeCount: nodeCountUsed,
		Mode:      "bootstrap-only",
		Phases:    queryResults,
	}

	reportPath := filepath.Join(artifactsDir, "scalability_report.md")
	reportFile, err := os.OpenFile(reportPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = reportFile.Close() }()

	err = tmpl.Execute(reportFile, reportData)
	Expect(err).NotTo(HaveOccurred())

	if os.Getenv("SKIP_TEARDOWN") == "true" {
		By("Skipping cluster teardown because SKIP_TEARDOWN is set to true")
		return
	}

	// Stop the kwok cluster cleanly
	By("Stopping the kwokctl cluster...")
	stopCmd := exec.Command(kwokctlBinaryPath, "stop", "cluster", "--name", "kwok") // #nosec G204
	stopOutput, err := utils.Run(stopCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to stop kwok cluster:\n%s", stopOutput)

	// Wait dynamically for Prometheus to shut down completely by verifying the port is closed
	Eventually(func(g Gomega) {
		_, err := http.Get("http://127.0.0.1:9090/-/ready")
		g.Expect(err).To(HaveOccurred()) // connection refused - Prometheus is down!
	}, "10s", "100ms").Should(Succeed(), "Prometheus failed to shut down")
})

func countTaintedNodes(ctx context.Context, clientset *kubernetes.Clientset) (int, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "type=kwok"})
	if err != nil {
		return 0, err
	}

	count := 0
	for _, node := range nodes.Items {
		for _, taint := range node.Spec.Taints {
			if taint.Key == "readiness.k8s.io/SecurityAgentNotReady" && taint.Value == "pending" {
				count++
				break
			}
		}
	}
	return count, nil
}

func countAnnotatedNodes(ctx context.Context, clientset *kubernetes.Clientset) (int, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "type=kwok"})
	if err != nil {
		return 0, err
	}

	count := 0
	for _, node := range nodes.Items {
		for k := range node.Annotations {
			if strings.HasPrefix(k, "readiness.k8s.io/bootstrap-completed-") {
				count++
				break
			}
		}
	}
	return count, nil
}



func doGetRequest(ctx context.Context, urlStr string) (*http.Response, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
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
	defer func() { _ = resp.Body.Close() }()

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

func collectAndReportMetricsForWindow(ctx context.Context, phaseTitle string, phaseStart time.Time, phaseEnd time.Time) (QueryResult, error) {
	// Sleep 2 seconds to ensure Prometheus scrapes the final data points
	time.Sleep(2 * time.Second)

	queryTime := phaseEnd.Add(2 * time.Second)
	lookbackSecs := int(queryTime.Sub(phaseStart).Seconds())
	if lookbackSecs < 10 {
		lookbackSecs = 10
	}
	ts := float64(queryTime.UnixNano()) / 1e9

	metricsMap := make(map[string]string)

	for _, q := range metricQueries {
		if q.PhaseFilter != "" && !strings.Contains(phaseTitle, q.PhaseFilter) {
			continue
		}

		queryStr := fmt.Sprintf(q.QueryTmpl, lookbackSecs, ts)
		val, err := queryPrometheusInstant(ctx, queryStr)
		if err != nil {
			if q.IsCounter {
				metricsMap[q.Key] = "0 " + q.Unit
			} else {
				metricsMap[q.Key] = "N/A"
			}
			continue
		}

		if q.Unit != "" {
			metricsMap[q.Key] = val + " " + q.Unit
		} else {
			metricsMap[q.Key] = val
		}
	}

	res := QueryResult{
		PhaseTitle:      phaseTitle,
		DurationSeconds: phaseEnd.Sub(phaseStart).Seconds(),
		Metrics:         metricsMap,
	}

	return res, nil
}
