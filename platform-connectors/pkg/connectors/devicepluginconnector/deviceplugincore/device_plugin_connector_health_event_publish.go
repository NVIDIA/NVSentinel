package deviceplugincore

import (
	"context"

	devicepluginpb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/devicepluginconnector/protos"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

func (r *DevicePluginConnector) ProcessHealthEvents(ctx context.Context, healthEvent *pb.HealthEvent) {
	devicePluginHealthEvent := devicepluginpb.HealthEvent{
		CheckName:          healthEvent.CheckName,
		IsHealthy:          healthEvent.IsHealthy,
		Message:            healthEvent.Message,
		ImpactedGPUIndices: append([]string(nil), healthEvent.EntitiesImpacted...),
		ErrorCode:          healthEvent.ErrorCode,
		IsFatal:            healthEvent.IsFatal,
		GeneratedTimestamp: healthEvent.GeneratedTimestamp,
		RecommendedAction:  devicepluginpb.RecommenedAction(healthEvent.RecommendedAction),
	}
	r.healthEventChan <- &devicePluginHealthEvent
}
