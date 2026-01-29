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

package evaluator

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type DrainEvaluator interface {
	// Database-agnostic method
	EvaluateEventWithDatabase(context.Context, model.HealthEventWithStatus, queue.DataStore,
		datastore.HealthEventStore) (*DrainActionResult, error)
}

type NodeDrainEvaluator struct {
	config            config.TomlConfig
	informers         InformersInterface
	customDrainClient CustomDrainClientInterface
}

type InformersInterface interface {
	GetNamespacesMatchingPattern(context.Context, string, string, string) ([]string, error)
	CheckIfAllPodsAreEvictedInImmediateMode(context.Context, []string, string, time.Duration, *protos.Entity) bool
	FindEvictablePodsInNamespaceAndNode(string, string, *protos.Entity) ([]*v1.Pod, error)
	GetNode(string) (*v1.Node, error)
}

type CustomDrainClientInterface interface {
	Exists(ctx context.Context, crName string) (bool, error)
	GetCRStatus(ctx context.Context, crName string) (bool, error)
}

type DrainAction int

const (
	ActionSkip DrainAction = iota
	ActionWait
	ActionCreateCR
	ActionEvictImmediate
	ActionEvictWithTimeout
	ActionCheckCompletion
	ActionMarkAlreadyDrained
	ActionUpdateStatus
)

type DrainActionResult struct {
	Action             DrainAction
	Namespaces         []string
	Timeout            time.Duration
	WaitDelay          time.Duration // For ActionWait
	Status             model.Status  // For ActionUpdateStatus
	PartialDrainEntity *protos.Entity
}

func (a DrainAction) String() string {
	switch a {
	case ActionSkip:
		return "Skip"
	case ActionWait:
		return "Wait"
	case ActionCreateCR:
		return "CreateCR"
	case ActionEvictImmediate:
		return "EvictImmediate"
	case ActionEvictWithTimeout:
		return "EvictWithTimeout"
	case ActionCheckCompletion:
		return "CheckCompletion"
	case ActionMarkAlreadyDrained:
		return "MarkAlreadyDrained"
	case ActionUpdateStatus:
		return "UpdateStatus"
	default:
		return "Unknown"
	}
}

type namespaces struct {
	immediateEvictionNamespaces  []string
	allowCompletionNamespaces    []string
	deleteAfterTimeoutNamespaces []string
}
