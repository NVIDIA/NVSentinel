// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package grpcsink

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	statusSuccess = "success"
	statusFailed  = "failed"
)

var (
	grpcSinkSendCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "grpc_sink_platform_connector_send_total",
		Help: "The total number of health event sends to the gRPC sink by status",
	}, []string{"status"})

	grpcSinkSendDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "grpc_sink_platform_connector_send_duration_milliseconds",
		Help:    "Duration of gRPC sink sends in milliseconds",
		Buckets: prometheus.ExponentialBuckets(10, 2, 12),
	})

	grpcSinkRetryCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "grpc_sink_platform_connector_retry_total",
		Help: "The total number of retried health event sends to the gRPC sink",
	})
)
