/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package conntrackstats

import "github.com/rhobs/operator-observability-toolkit/pkg/operatormetrics"

var (
	ConntrackMetrics = []operatormetrics.Metric{
		conntrackSyncExportsTotal,
		conntrackSyncExportedBytesTotal,
		conntrackSyncImportsTotal,
		conntrackSyncHookWaitsTotal,
	}

	conntrackSyncExportsTotal = operatormetrics.NewCounterVec(
		operatormetrics.MetricOpts{
			Name: "kubevirt_vmi_conntrack_sync_exports_total",
			Help: "Total conntrack sync export attempts during live migration, by result (success, skipped, error).",
		},
		[]string{"result"},
	)

	conntrackSyncExportedBytesTotal = operatormetrics.NewCounter(
		operatormetrics.MetricOpts{
			Name: "kubevirt_vmi_conntrack_sync_exported_bytes_total",
			Help: "Total bytes sent during conntrack sync for live migrations.",
		},
	)

	conntrackSyncImportsTotal = operatormetrics.NewCounterVec(
		operatormetrics.MetricOpts{
			Name: "kubevirt_vmi_conntrack_sync_imports_total",
			Help: "Total conntrack sync import attempts on the migration target, by result (success, aborted, error).",
		},
		[]string{"result"},
	)

	conntrackSyncHookWaitsTotal = operatormetrics.NewCounterVec(
		operatormetrics.MetricOpts{
			Name: "kubevirt_vmi_conntrack_sync_hook_waits_total",
			Help: "Total virt-launcher hook waits for conntrack injection, by result (completed, timeout).",
		},
		[]string{"result"},
	)
)

func RecordExportSuccess(bytes int) {
	conntrackSyncExportsTotal.WithLabelValues("success").Inc()
	conntrackSyncExportedBytesTotal.Add(float64(bytes))
}

func RecordExportSkipped() {
	conntrackSyncExportsTotal.WithLabelValues("skipped").Inc()
}

func RecordExportError() {
	conntrackSyncExportsTotal.WithLabelValues("error").Inc()
}

func RecordImportSuccess() {
	conntrackSyncImportsTotal.WithLabelValues("success").Inc()
}

func RecordImportAborted() {
	conntrackSyncImportsTotal.WithLabelValues("aborted").Inc()
}

func RecordImportError() {
	conntrackSyncImportsTotal.WithLabelValues("error").Inc()
}

func RecordHookWaitCompleted() {
	conntrackSyncHookWaitsTotal.WithLabelValues("completed").Inc()
}

func RecordHookWaitTimeout() {
	conntrackSyncHookWaitsTotal.WithLabelValues("timeout").Inc()
}
