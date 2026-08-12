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

package dedup

import (
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const noErrorCodeLabel = "none"

var dedupStoreAndAnalyseCounter = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "nvsentinel_platform_connector_dedup_store_and_analyse_total",
		Help: "Total number of duplicate health events marked STORE_AND_ANALYSE by deduplication.",
	},
	[]string{"check", "node", "err_code"},
)

func errCodeLabel(event *pb.HealthEvent) string {
	errorCodes := append([]string(nil), event.GetErrorCode()...)
	if len(errorCodes) == 0 {
		return noErrorCodeLabel
	}

	sort.Strings(errorCodes)

	return strings.Join(errorCodes, ",")
}
