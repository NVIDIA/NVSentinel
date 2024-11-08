package nodehealtheventsudscore

import (
	"context"

	nodeHealthEventsPluginPb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

func mapEntitiesToCorrectType(entities []*pb.Entity) []*nodeHealthEventsPluginPb.Entity {
	var result []*nodeHealthEventsPluginPb.Entity
	for _, entity := range entities {
		result = append(result, &nodeHealthEventsPluginPb.Entity{
			EntityType:  entity.EntityType,
			EntityValue: entity.EntityValue,
		})
	}

	return result
}

func (r *NodeHealthEventsUDSConnector) ProcessHealthEvents(ctx context.Context, healthEvent *pb.HealthEvent) {
	nodeHealthEvent := nodeHealthEventsPluginPb.HealthEvent{
		CheckName:          healthEvent.CheckName,
		IsHealthy:          healthEvent.IsHealthy,
		Message:            healthEvent.Message,
		ImpactedIndices:    mapEntitiesToCorrectType(healthEvent.EntitiesImpacted),
		ErrorCode:          healthEvent.ErrorCode,
		IsFatal:            healthEvent.IsFatal,
		GeneratedTimestamp: healthEvent.GeneratedTimestamp,
		RecommendedAction:  nodeHealthEventsPluginPb.RecommenedAction(healthEvent.RecommendedAction),
	}
	r.healthEventChan <- &nodeHealthEvent
}
