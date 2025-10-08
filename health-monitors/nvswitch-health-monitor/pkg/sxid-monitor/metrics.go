//go:build linux

/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
 */

package sxid_monitor

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	kernelLogsProcessed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nvswitch_monitor_kernel_logs_processed_total",
		Help: "The total number of kernel logs processed (includes failed and succeeded)",
	})

	sxidLogsProcessingFailed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nvswitch_monitor_sxid_logs_failed_total",
		Help: "The total number of SXID logs failed to process",
	})

	sxidLogsProcessingSucceeded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nvswitch_monitor_sxid_logs_succeeded_total",
		Help: "The total number of SXID logs successfully processed",
	})

	syslogReadCallsSucceeded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nvswitch_monitor_syslog_read_calls_succeeded_total",
		Help: "The total number of successful syslog buffer read calls",
	})

	syslogReadCallsFailed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nvswitch_monitor_syslog_read_calls_failed_total",
		Help: "The total number of failed syslog buffer read calls",
	})

	pollingLoopProcessingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "nvswitch_monitor_polling_loop_processing_duration_milliseconds",
		Help:    "The processing time for each polling loop in milliseconds (excluding the polling interval wait time)",
		Buckets: prometheus.LinearBuckets(0, 10, 500),
	})
)
