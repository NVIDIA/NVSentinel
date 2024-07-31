package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	lsnvlink "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nvswitch-health-monitor/pkg/lsnvlink"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nvswitch-health-monitor/pkg/protos"
	sxid "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nvswitch-health-monitor/pkg/sxid-monitor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog"
)

const (
	AGENT           = "nvswitch-health-monitor"
	CHECK_NAME      = "NvswitchErrorFromKmsgWatch"
	COMPONENT_CLASS = "nvswitch"
)

const defaultStateFilePath = "/var/run/nvswitch_monitor/state.json"

func GetGPUID(nvswitch, nvlink int) (int, error) {
	dgxType := lsnvlink.GetDGXType()

	if dgxType == lsnvlink.DGX_TYPE_A100 {
		return lsnvlink.DGX_A100{}.GetGpuFromNVSwitchNVLink(nvswitch, nvlink)
	} else if dgxType == lsnvlink.DGX_TYPE_H100 {
		return lsnvlink.DGX_H100{}.GetGpuFromNVSwitchNVLink(nvswitch, nvlink)
	}
	return -1, errors.New("failed to get gpu id associated, dgx type is unknown")
}

func SxidError2HealthEvents(sxidError *sxid.SXIDErrorEvent) *pb.HealthEvents {
	healthEvents := pb.HealthEvents{Version: 1, Events: make([]*pb.HealthEvent, 0)}

	entitiesImpacted := []string{
		fmt.Sprintf("nvswitch%d", sxidError.NVSwitch),
		sxidError.PCI,
		fmt.Sprintf("nvlink%d", sxidError.Link),
	}
	gpuID, err := GetGPUID(sxidError.NVSwitch, sxidError.Link)

	if err != nil {
		entitiesImpacted = append(entitiesImpacted, fmt.Sprintf("gpu%d", gpuID))
	}

	event := pb.HealthEvent{
		Version:            1,
		Agent:              AGENT,
		CheckName:          CHECK_NAME,
		ComponentClass:     COMPONENT_CLASS,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		IsFatal:            sxidError.IsFatal,
		ErrorCode:          []string{fmt.Sprint(sxidError.ErrorNum)},
		EntitiesImpacted:   entitiesImpacted,
		Message:            sxidError.Message,
		// ActionRequired:     false,
		// RecommendedAction:  pb.RecommenedAction_UNKNOWN,
	}

	healthEvents.Events = append(healthEvents.Events, &event)

	return &healthEvents
}

func main() {
	var socket = flag.String("socket", "unix:///var/run/nvsentinel.sock", "unix domain socket")
	flag.Parse()

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	conn, err := grpc.NewClient(*socket, opts...)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	client := pb.NewPlatformConnectorClient(conn)

	sxidErrorMonitor, err := sxid.NewSxidErrorMonitor(defaultStateFilePath)
	if err != nil {
		panic(err)
	}
	defer sxidErrorMonitor.Close()

	errChan := make(chan error, 1)
	go func() {
		errChan <- sxidErrorMonitor.Run()
	}()

	for {
		select {
		case err := <-errChan:
			panic(err)
		case sxidError := <-sxidErrorMonitor.EventChan:
			healthEvents := SxidError2HealthEvents(sxidError)
			_, err := client.HealthEventOccuredV1(context.Background(), healthEvents)
			if err != nil {
				klog.Error(err)
			}
		}
	}
}
