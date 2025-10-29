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

package nodemetadata

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	augmentationTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nodemetadata_augmentation_total",
		Help: "Total number of health event augmentation attempts",
	})

	augmentationSuccess = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nodemetadata_augmentation_success_total",
		Help: "Total number of successful health event augmentations",
	})

	augmentationFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nodemetadata_augmentation_failures_total",
		Help: "Total number of failed health event augmentations",
	})

	augmentationDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "nodemetadata_augmentation_duration_milliseconds",
		Help:    "Duration of health event augmentation in milliseconds",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	})

	cacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nodemetadata_cache_hits_total",
		Help: "Total number of cache hits",
	})

	cacheMisses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nodemetadata_cache_misses_total",
		Help: "Total number of cache misses",
	})

	cacheSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "nodemetadata_cache_size",
		Help: "Current number of entries in the cache",
	})

	cacheEvictions = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nodemetadata_cache_evictions_total",
		Help: "Total number of cache evictions",
	})

	k8sAPICallsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nodemetadata_k8s_api_calls_total",
		Help: "Total number of Kubernetes API calls",
	})

	k8sAPICallsSuccess = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nodemetadata_k8s_api_calls_success_total",
		Help: "Total number of successful Kubernetes API calls",
	})

	k8sAPICallsFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nodemetadata_k8s_api_calls_failures_total",
		Help: "Total number of failed Kubernetes API calls",
	})

	k8sAPICallDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "nodemetadata_k8s_api_call_duration_milliseconds",
		Help:    "Duration of Kubernetes API calls in milliseconds",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	})
)

