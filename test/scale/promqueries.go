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

// MetricQuery defines a query format for scraping Prometheus during scale tests.
type MetricQuery struct {
	Key         string
	QueryTmpl   string
	Unit        string
	PhaseFilter string
	IsCounter   bool
}

// metricQueries contains the definitions of all Prometheus queries used in the scale tests.
var metricQueries = []MetricQuery{
	{
		Key:       "reconcile_time_p50",
		QueryTmpl: "histogram_quantile(0.50, sum(rate(controller_runtime_reconcile_time_seconds_bucket{controller=\"node\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "reconcile_time_p90",
		QueryTmpl: "histogram_quantile(0.90, sum(rate(controller_runtime_reconcile_time_seconds_bucket{controller=\"node\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "reconcile_time_p99",
		QueryTmpl: "histogram_quantile(0.99, sum(rate(controller_runtime_reconcile_time_seconds_bucket{controller=\"node\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "reconciliation_latency_p50",
		QueryTmpl: "histogram_quantile(0.50, sum(rate(node_readiness_reconciliation_latency_seconds_bucket{rule=\"security-agent-readiness-rule\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "reconciliation_latency_p90",
		QueryTmpl: "histogram_quantile(0.90, sum(rate(node_readiness_reconciliation_latency_seconds_bucket{rule=\"security-agent-readiness-rule\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "reconciliation_latency_p99",
		QueryTmpl: "histogram_quantile(0.99, sum(rate(node_readiness_reconciliation_latency_seconds_bucket{rule=\"security-agent-readiness-rule\"}[%ds])) by (le))",
		Unit:      "s",
	},

	{
		Key:       "workqueue_queue_duration_p50",
		QueryTmpl: "histogram_quantile(0.50, sum(rate(workqueue_queue_duration_seconds_bucket{name=\"node\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "workqueue_queue_duration_p90",
		QueryTmpl: "histogram_quantile(0.90, sum(rate(workqueue_queue_duration_seconds_bucket{name=\"node\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "workqueue_queue_duration_p99",
		QueryTmpl: "histogram_quantile(0.99, sum(rate(workqueue_queue_duration_seconds_bucket{name=\"node\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "workqueue_work_duration_p50",
		QueryTmpl: "histogram_quantile(0.50, sum(rate(workqueue_work_duration_seconds_bucket{name=\"node\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "workqueue_work_duration_p90",
		QueryTmpl: "histogram_quantile(0.90, sum(rate(workqueue_work_duration_seconds_bucket{name=\"node\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "workqueue_work_duration_p99",
		QueryTmpl: "histogram_quantile(0.99, sum(rate(workqueue_work_duration_seconds_bucket{name=\"node\"}[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "rule_evaluation_duration_p50",
		QueryTmpl: "histogram_quantile(0.50, sum(rate(node_readiness_evaluation_duration_seconds_bucket[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "rule_evaluation_duration_p90",
		QueryTmpl: "histogram_quantile(0.90, sum(rate(node_readiness_evaluation_duration_seconds_bucket[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "rule_evaluation_duration_p99",
		QueryTmpl: "histogram_quantile(0.99, sum(rate(node_readiness_evaluation_duration_seconds_bucket[%ds])) by (le))",
		Unit:      "s",
	},
	{
		Key:       "taint_operations_add",
		QueryTmpl: "sum(increase(node_readiness_taint_operations_total{rule=\"security-agent-readiness-rule\",operation=\"add\"}[%ds]))",
		Unit:      "ops",
		IsCounter: true,
	},
	{
		Key:       "taint_operations_remove",
		QueryTmpl: "sum(increase(node_readiness_taint_operations_total{rule=\"security-agent-readiness-rule\",operation=\"remove\"}[%ds]))",
		Unit:      "ops",
		IsCounter: true,
	},

	{
		Key:       "condition_failures_total",
		QueryTmpl: "sum(increase(node_readiness_condition_failures_total{rule=\"security-agent-readiness-rule\"}[%ds]))",
		Unit:      "failures",
		IsCounter: true,
	},
	{
		Key:       "operational_failures_total",
		QueryTmpl: "sum(increase(node_readiness_failures_total{rule=\"security-agent-readiness-rule\"}[%ds]))",
		Unit:      "failures",
		IsCounter: true,
	},
	{
		Key:       "cpu_cores_rate",
		QueryTmpl: "sum(rate(process_cpu_seconds_total[%ds]))",
		Unit:      "cores",
	},
	{
		Key:       "resident_memory_peak",
		QueryTmpl: "max(max_over_time(process_resident_memory_bytes[%ds]))",
		Unit:      "bytes",
	},
}
