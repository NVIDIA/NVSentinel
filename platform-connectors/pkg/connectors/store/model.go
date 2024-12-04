// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package store

import (
	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

type HealthEventStatus struct {
	NodeQuarantined         bool `bson:"nodequarantined"`
	UserPodsEvictedFromNode bool `bson:"userpodsevictedfromnode"`
	FaultRemediated         bool `bson:"faultremediated"`
}

type HealthEventWithStatus struct {
	HealthEvent       *platformconnector.HealthEvent `bson:"healthevent,omitempty"`
	HealthEventStatus HealthEventStatus              `bson:"healtheventstatus"`
}
