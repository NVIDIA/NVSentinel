package nodehealtheventsudscore

import (
	"context"
	"time"

	nodeHealthEventsPluginPb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"k8s.io/klog"
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

func (r *NodeHealthEventsUDSConnector) ProcessHealthEvents(ctx context.Context, healthEvents *pb.HealthEvents) {
	nodeHealthEvents := nodeHealthEventsPluginPb.HealthEvents{Version: 1,
		Events: make([]*nodeHealthEventsPluginPb.HealthEvent, 0)}

	for _, healthEvent := range healthEvents.Events {
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
		nodeHealthEvents.Events = append(nodeHealthEvents.Events, &nodeHealthEvent)
	}
	select {
	case r.healthEventChan <- &nodeHealthEvents:
	case <-time.After(100 * time.Millisecond):
		for len(r.healthEventChan) > 0 {
			<-r.healthEventChan
		}

		klog.Infof("Sending the healthEvents again after clearing the channel")
		r.healthEventChan <- &nodeHealthEvents
	}
}
