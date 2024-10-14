package nodehealtheventsudscore

import (
	"context"

	nodeHealthEventsPluginPb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

func (r *NodeHealthEventsUDSConnector) ProcessHealthEvents(ctx context.Context, healthEvent *pb.HealthEvent) {
	nodeHealthEvent := nodeHealthEventsPluginPb.HealthEvent{
		CheckName:          healthEvent.CheckName,
		IsHealthy:          healthEvent.IsHealthy,
		Message:            healthEvent.Message,
		ImpactedGPUIndices: append([]string(nil), healthEvent.EntitiesImpacted...),
		ErrorCode:          healthEvent.ErrorCode,
		IsFatal:            healthEvent.IsFatal,
		GeneratedTimestamp: healthEvent.GeneratedTimestamp,
		RecommendedAction:  nodeHealthEventsPluginPb.RecommenedAction(healthEvent.RecommendedAction),
	}
	r.healthEventChan <- &nodeHealthEvent
}
