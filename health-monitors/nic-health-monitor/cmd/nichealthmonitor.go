package main

import (
	"context"
	"flag"
	"time"

	nic "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nic-health-monitor/pkg/nic-monitor"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nic-health-monitor/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog"
)

const (
	AGENT                      = "nic-health-monitor"
	INFINIBAND_CHECK_NAME      = "InfiniBandErrorCheck"
	INFINIBAND_COMPONENT_CLASS = "infiniBand"

	ETHERNET_CHECK_NAME      = "EthernetErrorCheck"
	ETHERNET_COMPONENT_CLASS = "ethernet"
)

func NicError2HealthEvents(nicErrors *[]nic.NicErrorEvent) *pb.HealthEvents {
	healthEvents := pb.HealthEvents{Version: 1, Events: make([]*pb.HealthEvent, 0)}

	for _, nicError := range *nicErrors {
		var checkname, componentClass string

		if nicError.NicType == nic.Infiniband {
			checkname = INFINIBAND_CHECK_NAME
			componentClass = INFINIBAND_COMPONENT_CLASS
		} else if nicError.NicType == nic.Ethernet {
			checkname = ETHERNET_CHECK_NAME
			componentClass = ETHERNET_COMPONENT_CLASS
		}

		event := pb.HealthEvent{
			Version:            1,
			Agent:              AGENT,
			CheckName:          checkname,
			ComponentClass:     componentClass,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			EntitiesImpacted:   []string{nicError.Name},
			Message:            nicError.Message,
			// IsFatal:            nicError.IsFatal,
			// ErrorCode:          fmt.Sprint(nicError.ErrorNum),
			// ActionRequired:     false,
			// RecommendedAction:  pb.RecommenedAction_UNKNOWN,
		}

		healthEvents.Events = append(healthEvents.Events, &event)
	}

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

	nicErrorMonitor, err := nic.NewNicErrorMonitor()
	if err != nil {
		panic(err)
	}

	errChan := make(chan error, 1)
	go func() {
		errChan <- nicErrorMonitor.Run()
	}()

	for {
		select {
		case err := <-errChan:
			panic(err)
		case nicError := <-nicErrorMonitor.EventChan:
			healthEvents := NicError2HealthEvents(nicError)

			_, err := client.HealthEventOccuredV1(context.Background(), healthEvents)
			if err != nil {
				klog.Error(err)
			} else {
				klog.Infof("Successfully sent health events: %+v", healthEvents)
			}
		}
	}
}
