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
