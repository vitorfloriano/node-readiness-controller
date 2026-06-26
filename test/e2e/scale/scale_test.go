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
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	readinessv1alpha1 "sigs.k8s.io/node-readiness-controller/api/v1alpha1"
)

var _ = Describe("Node Readiness Controller Scale Tests", func() {
	var (
		cmd        *exec.Cmd
		clientset  *kubernetes.Clientset
		k8sClient  client.Client
		kubeCtx    context.Context
		kubeCancel context.CancelFunc
		projectDir string
	)

	BeforeEach(func() {
		kubeCtx, kubeCancel = context.WithCancel(context.Background())

		// Resolve project root
		wd, err := os.Getwd()
		Expect(err).NotTo(HaveOccurred())
		projectDir = filepath.Clean(filepath.Join(wd, "..", "..", ".."))

		// Create clientset and controller-runtime client
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		Expect(err).NotTo(HaveOccurred(), "Failed to build client-go config")
		clientset, err = kubernetes.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred(), "Failed to create clientset")

		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(readinessv1alpha1.AddToScheme(scheme)).To(Succeed())

		k8sClient, err = client.New(config, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred(), "Failed to create controller-runtime client")

		// Apply KWOK stages for custom readiness simulation
		By("applying KWOK stages for custom readiness simulation")
		applyStageCmd := exec.Command("kubectl", "apply",
			"-f", filepath.Join(projectDir, "test/e2e/scale/stage-init.yaml"),
			"-f", filepath.Join(projectDir, "test/e2e/scale/stage-transition.yaml"),
		)
		applyStageCmd.Env = append(applyStageCmd.Environ(), "KUBECONFIG="+kubeconfigPath)
		output, err := applyStageCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to apply KWOK stages: %s", string(output)))

		// Start the controller manager process
		By("starting the controller manager process")
		cmd = exec.Command(controllerBinaryPath,
			"--metrics-bind-address=:8080",
			"--health-probe-bind-address=:8081",
			"--leader-elect=false",
			"--enable-webhook=false",
		)
		cmd.Env = append(cmd.Environ(), "KUBECONFIG="+kubeconfigPath)

		// Start the process asynchronously
		err = cmd.Start()
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller manager process")

		// Wait for the metrics endpoint to become ready
		By("waiting for the controller metrics port to bind")
		Eventually(func(g Gomega) {
			resp, err := http.Get("http://localhost:8080/metrics")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
		}, "10s", "500ms").Should(Succeed(), "Controller failed to start or bind port 8080")
	})

	AfterEach(func() {
		// Clean up the controller process
		if cmd != nil && cmd.Process != nil {
			By("stopping the controller manager process")
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}

		if kubeCancel != nil {
			kubeCancel()
		}

		// Delete rule
		By("deleting NodeReadinessRule")
		rule := &readinessv1alpha1.NodeReadinessRule{
			ObjectMeta: metav1.ObjectMeta{
				Name: "scale-test-rule",
			},
		}
		_ = k8sClient.Delete(context.Background(), rule)

		// Delete stages
		By("cleaning up KWOK stages")
		deleteStageCmd := exec.Command("kubectl", "delete",
			"-f", filepath.Join(projectDir, "test/e2e/scale/stage-init.yaml"),
			"-f", filepath.Join(projectDir, "test/e2e/scale/stage-transition.yaml"),
			"--ignore-not-found",
		)
		deleteStageCmd.Env = append(deleteStageCmd.Environ(), "KUBECONFIG="+kubeconfigPath)
		_ = deleteStageCmd.Run()

		// Clean up nodes scaled during the test
		By("cleaning up simulated nodes")
		cleanupCmd := exec.Command("kwokctl", "scale", "node", "kwok-node", "--replicas", "0", "--name", "nrr-scale-test")
		_ = cleanupCmd.Run()

		// Delete base node
		By("deleting base node")
		baseNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kwok-node",
			},
		}
		_ = k8sClient.Delete(context.Background(), baseNode)
	})

	var sizes []int
	if val, ok := os.LookupEnv("SCALE_NODES_COUNT"); ok {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
			sizes = []int{parsed}
		}
	} else {
		// Run scaling test matrix dynamically from 10 to 100 to 500 to 1000 nodes
		sizes = []int{10, 100, 500, 1000}
	}

	for _, size := range sizes {
		nodeCount := size
		It(fmt.Sprintf("should reconcile scaled nodes, remove taints, and meet SLOs for %d nodes", nodeCount), func() {
			totalExpectedNodes := nodeCount + 1

		// Create NodeReadinessRule
		By("creating NodeReadinessRule")
		rule := &readinessv1alpha1.NodeReadinessRule{
			ObjectMeta: metav1.ObjectMeta{
				Name: "scale-test-rule",
			},
			Spec: readinessv1alpha1.NodeReadinessRuleSpec{
				NodeSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"type": "kwok",
					},
				},
				EnforcementMode: readinessv1alpha1.EnforcementModeContinuous,
				Taint: corev1.Taint{
					Key:    "readiness.k8s.io/cni-not-ready",
					Effect: corev1.TaintEffectNoSchedule,
					Value:  "true",
				},
				Conditions: []readinessv1alpha1.ConditionRequirement{
					{
						Type:           "custom/Ready",
						RequiredStatus: corev1.ConditionTrue,
					},
				},
			},
		}
		Expect(k8sClient.Create(kubeCtx, rule)).To(Succeed(), "Failed to create NodeReadinessRule")

		By("creating a base node template")
		baseNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kwok-node",
				Labels: map[string]string{
					"type":               "kwok",
					"kwok.x-k8s.io/node": "fake",
				},
			},
			Spec: corev1.NodeSpec{
				ProviderID: "kwok://kwok-node",
			},
		}
		err := k8sClient.Create(kubeCtx, baseNode)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred(), "Failed to create base node")
		}

		By(fmt.Sprintf("scaling simulated nodes to %d", nodeCount))
		scaleCmd := exec.Command("kwokctl", "scale", "node", "kwok-node",
			"--replicas", fmt.Sprintf("%d", nodeCount),
			"--name", "nrr-scale-test",
		)
		output, err := scaleCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to scale nodes: %s", string(output)))

		By("waiting for all nodes to become custom/Ready=True and have taints removed")
		Eventually(func(g Gomega) {
			nodes, err := clientset.CoreV1().Nodes().List(kubeCtx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			kwokNodes := []corev1.Node{}
			for _, node := range nodes.Items {
				if node.Labels["type"] == "kwok" {
					kwokNodes = append(kwokNodes, node)
				}
			}

			// We expect exactly totalExpectedNodes nodes to be present (base template + generated replicas)
			g.Expect(len(kwokNodes)).To(Equal(totalExpectedNodes))

			// Check conditions and taints on each node
			for _, node := range kwokNodes {
				// Verify custom/Ready condition is True
				var customReadyStatus corev1.ConditionStatus = corev1.ConditionUnknown
				for _, cond := range node.Status.Conditions {
					if cond.Type == "custom/Ready" {
						customReadyStatus = cond.Status
						break
					}
				}
				g.Expect(customReadyStatus).To(Equal(corev1.ConditionTrue), fmt.Sprintf("Node %s is not custom/Ready yet", node.Name))

				// Verify taint is removed
				hasTaint := false
				for _, taint := range node.Spec.Taints {
					if taint.Key == "readiness.k8s.io/cni-not-ready" {
						hasTaint = true
						break
					}
				}
				g.Expect(hasTaint).To(BeFalse(), fmt.Sprintf("Node %s still has readiness taint", node.Name))
			}
		}, "30s", "1s").Should(Succeed())

		By("scraping and saving metrics from the controller manager")
		metricFamilies, err := scrapeAndSaveMetrics(
			filepath.Join(projectDir, "test/e2e/scale/artifacts"),
			fmt.Sprintf("scale-%d.prom", nodeCount),
		)
		Expect(err).NotTo(HaveOccurred(), "Failed to scrape controller metrics")

		// Verify Correctness: Taint Adds == Taint Removes == Node Count
		By("asserting metric correctness and counts")
		adds := getCounterValue(metricFamilies, "node_readiness_taint_operations_total", map[string]string{
			"rule":      "scale-test-rule",
			"operation": "add",
		})
		removes := getCounterValue(metricFamilies, "node_readiness_taint_operations_total", map[string]string{
			"rule":      "scale-test-rule",
			"operation": "remove",
		})
		failures := getCounterValue(metricFamilies, "node_readiness_failures_total", map[string]string{
			"rule": "scale-test-rule",
		})

		Expect(adds).To(Equal(float64(totalExpectedNodes)), "Adds should equal total node count")
		Expect(removes).To(Equal(float64(totalExpectedNodes)), "Removes should equal total node count")
		Expect(failures).To(Equal(0.0), "Operational failures should be zero")

		// Verify SLO: p99 evaluation duration is < 50ms
		By("verifying performance SLOs")
		p99Ok := verifyEvaluationDurationSLO(metricFamilies, 0.05, 0.99)
		Expect(p99Ok).To(BeTrue(), "p99 of evaluation duration must be less than 50ms")
		})
	}
})

func scrapeAndSaveMetrics(artifactsDir string, filename string) (map[string]*dto.MetricFamily, error) {
	resp, err := http.Get("http://localhost:8080/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(artifactsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create artifacts directory: %w", err)
	}

	promFilePath := filepath.Join(artifactsDir, filename)
	if err := os.WriteFile(promFilePath, bodyBytes, 0644); err != nil {
		return nil, fmt.Errorf("failed to write metrics file: %w", err)
	}

	var parser expfmt.TextParser
	metricFamilies, err := parser.TextToMetricFamilies(bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	return metricFamilies, nil
}

func getCounterValue(metricFamilies map[string]*dto.MetricFamily, name string, targetLabels map[string]string) float64 {
	family, exists := metricFamilies[name]
	if !exists {
		return 0
	}
	for _, m := range family.Metric {
		if labelsMatch(m.Label, targetLabels) {
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

func labelsMatch(labels []*dto.LabelPair, target map[string]string) bool {
	matched := 0
	for k, v := range target {
		for _, lp := range labels {
			if lp.GetName() == k && lp.GetValue() == v {
				matched++
				break
			}
		}
	}
	return matched == len(target)
}

func verifyEvaluationDurationSLO(metricFamilies map[string]*dto.MetricFamily, thresholdSeconds float64, targetRatio float64) bool {
	family, exists := metricFamilies["node_readiness_evaluation_duration_seconds"]
	if !exists {
		return false
	}
	for _, m := range family.Metric {
		hist := m.GetHistogram()
		if hist == nil {
			continue
		}
		totalCount := hist.GetSampleCount()
		if totalCount == 0 {
			return true // No evaluations recorded, trivially satisfies SLO
		}

		var countUnderThreshold uint64
		foundBucket := false
		for _, bucket := range hist.Bucket {
			if bucket.GetUpperBound() <= thresholdSeconds {
				countUnderThreshold = bucket.GetCumulativeCount()
				foundBucket = true
			}
		}

		if !foundBucket {
			return false
		}

		ratio := float64(countUnderThreshold) / float64(totalCount)
		return ratio >= targetRatio
	}
	return false
}
