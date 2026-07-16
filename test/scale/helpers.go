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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/node-readiness-controller/test/utils"
)

type kwokNodeList struct {
	Items []struct {
		Spec struct {
			Taints []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"taints"`
		} `json:"spec"`
	} `json:"items"`
}

type prometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type QueryResult struct {
	PhaseTitle      string            `json:"phase_title"`
	DurationSeconds float64           `json:"duration_seconds"`
	Metrics         map[string]string `json:"metrics"`
}

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

func getKwokNodes(ctx context.Context) (*kwokNodeList, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "nodes", "-l", "type=kwok", "-o", "json")
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, err
	}

	var list kwokNodeList
	if err := json.Unmarshal([]byte(output), &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func countTaintedNodes(ctx context.Context) (int, error) {
	list, err := getKwokNodes(ctx)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, node := range list.Items {
		for _, taint := range node.Spec.Taints {
			if taint.Key == "readiness.k8s.io/SecurityAgentNotReady" && taint.Value == "pending" {
				count++
				break
			}
		}
	}
	return count, nil
}

func queryPrometheusInstant(ctx context.Context, query string, ts float64) (string, error) {
	urlStr := fmt.Sprintf("http://127.0.0.1:%s/api/v1/query?query=%s&time=%.3f", prometheusPort, url.QueryEscape(query), ts)

	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	var promResp prometheusResponse

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

		queryStr := fmt.Sprintf(q.QueryTmpl, lookbackSecs)
		val, err := queryPrometheusInstant(ctx, queryStr, ts)
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
