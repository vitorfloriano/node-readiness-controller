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

package scale

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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


	It("should successfully run the scale test phases and evaluate performance", func() {
		queryResults = nil
		ctx := context.Background()
		nodeCount := nodeCountUsed

		var phases []PhaseStats

		// Tainting Phase
		taintStart := time.Now()

		By("Applying Security Agent NodeReadinessRule resource")
		setupRuleCmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		setupRuleCmd.Stdin = strings.NewReader(securityAgentRuleManifest)
		setupRuleCmdOutput, err := utils.Run(setupRuleCmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply Security Agent NodeReadinessRule manifest:\n%s", setupRuleCmdOutput)

		By("Applying security-agent-stage-false stage rules to simulate unhealthy agent status")
		applyFalseCmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		applyFalseCmd.Stdin = strings.NewReader(securityAgentStageFalseManifest)
		falseOutput, err := utils.Run(applyFalseCmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply false stage: %s", falseOutput)

		By("Waiting for the controller manager to reconcile and add taints to all nodes")
		Eventually(func(g Gomega) int {
			count, err := countTaintedNodes(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			By(fmt.Sprintf("Progress: %d/%d nodes successfully tainted", count, nodeCount))
			return count
		}, "15m", "10s").Should(Equal(nodeCount), "Tainted nodes count did not reach target replicas")

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

		applyTrueCmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		applyTrueCmd.Stdin = strings.NewReader(securityAgentStageTrueManifest)
		trueOutput, err := utils.Run(applyTrueCmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to apply true stage: %s", trueOutput)

		By("Waiting for the controller manager to reconcile and remove taints on all nodes")
		Eventually(func(g Gomega) {
			tainted, err := countTaintedNodes(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tainted).To(Equal(0))

			if strings.Contains(securityAgentRuleManifest, "bootstrap-only") {
				annotated, err := countAnnotatedNodes(ctx)
				g.Expect(err).NotTo(HaveOccurred())
				By(fmt.Sprintf("Progress: %d/%d nodes remaining tainted, %d/%d nodes annotated", tainted, nodeCount, annotated, nodeCount))
				g.Expect(annotated).To(Equal(nodeCount))
			} else {
				By(fmt.Sprintf("Progress: %d/%d nodes remaining tainted (continuous mode, skipping annotation check)", tainted, nodeCount))
			}
		}, "15m", "10s").Should(Succeed(), "Failed to complete untainting phase")

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
		Mode:      "continuous",
		Phases:    queryResults,
	}

	reportPath := filepath.Join(artifactsDir, "scalability_report.md")
	reportFile, err := os.OpenFile(reportPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = reportFile.Close() }()

	err = tmpl.Execute(reportFile, reportData)
	Expect(err).NotTo(HaveOccurred())

	if os.Getenv("SKIP_TEARDOWN") != "true" {
		// Terminate the background controller process
		By("Terminating the background controller process")
		if controllerCmd != nil && controllerCmd.Process != nil {
			_ = controllerCmd.Process.Kill()
			_ = controllerCmd.Wait()
		}
		if controllerLogFile != nil {
			_ = controllerLogFile.Close()
		}
	}

	if os.Getenv("SKIP_TEARDOWN") == "true" {
		By("Skipping cluster teardown because SKIP_TEARDOWN is set to true")
		return
	}

	// Delete the kwok cluster cleanly
	By("Deleting the kwokctl cluster...")
	deleteCmd := exec.Command(kwokctlBinaryPath, "delete", "cluster", "--name", "kwok") // #nosec G204
	deleteOutput, err := utils.Run(deleteCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to delete kwok cluster:\n%s", deleteOutput)

	// Wait dynamically for Prometheus to shut down completely by verifying the port is closed
	Eventually(func(g Gomega) {
		_, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/-/ready", prometheusPort))
		g.Expect(err).To(HaveOccurred()) // connection refused - Prometheus is down!
	}, "10s", "100ms").Should(Succeed(), "Prometheus failed to shut down")
})

