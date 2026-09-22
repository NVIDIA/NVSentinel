// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package auth

import (
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/kubeconfig"
)

// tokenReviewTimeout bounds one TokenReview call. client-go sets none, and a
// wedged API server would otherwise hold the call, and the caller's batch,
// open indefinitely instead of letting the caller retry.
const tokenReviewTimeout = 10 * time.Second

// NewTokenReviewValidator builds the TokenReview validator both roles of the
// platform connector authenticate callers with. An empty kubeconfigPath means
// the in-cluster configuration. qps and burst size the Kubernetes client for
// a call on the path of every batch, not for a controller that writes
// occasionally: at client-go's defaults (5 QPS, 10 burst) a burst of events
// would queue in the client's rate limiter.
//
// The validator answers authentication only; which node an identity may name
// is the node-binding interceptor's decision. Its client is deliberately not
// wrapped with the audit logger. A TokenReview is a question, not a change to
// the cluster, so there is nothing for a change audit to record. It is only a
// POST because that is the shape of the API, and the audit round tripper
// treats every POST as a write: it would emit an entry per authenticated
// batch, and with AUDIT_LOG_REQUEST_BODY on it would copy the request body,
// the caller's ServiceAccount token, into the log.
func NewTokenReviewValidator(
	kubeconfigPath, audience string, qps float32, burst int, opts ...grpcauth.ValidatorOption,
) (*grpcauth.Validator, error) {
	restConfig, err := kubeconfig.Load(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubernetes auth configuration: %w", err)
	}

	restConfig.QPS = qps
	restConfig.Burst = burst
	restConfig.Timeout = tokenReviewTimeout

	clientSet, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes clientset for auth: %w", err)
	}

	validator, err := grpcauth.NewValidator(clientSet, audience, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to build token validator: %w", err)
	}

	return validator, nil
}
