package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

// prometheus metrics
var (
	healthEventsPublished = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nvswitch_monitor_health_events_published_total",
		Help: "The total number of health events that the nvswitch monitor has raised",
	})

	gpuIdCalculationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nvswitch_monitor_gpu_id_calculation_duration_milliseconds",
		Help:    "The time taken for calculating GPU ID for each sxid event in milliseconds",
		Buckets: prometheus.LinearBuckets(0, 10, 500),
	}, []string{"gpu_id"})

	healthEventPublishDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "nvswitch_monitor_health_event_publish_duration_milliseconds",
		Help:    "The time taken by nvswitch monitor to publish health event in milliseconds",
		Buckets: prometheus.LinearBuckets(0, 10, 500),
	})
)

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
	start := time.Now()

	gpuID, err := GetGPUID(sxidError.NVSwitch, sxidError.Link)

	duration := float64(time.Since(start).Milliseconds())
	gpuIdCalculationDuration.With(prometheus.Labels{"gpu_id": fmt.Sprint(gpuID)}).Observe(duration)

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

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

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

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		err := http.ListenAndServe(":"+*metricsPort, nil)
		if err != nil {
			klog.Fatalf("Failed to start metrics server: %v", err)
		}
	}()

	for {
		select {
		case err := <-errChan:
			panic(err)
		case sxidError := <-sxidErrorMonitor.EventChan:
			healthEvents := SxidError2HealthEvents(sxidError)
			start := time.Now()
			_, err := client.HealthEventOccuredV1(context.Background(), healthEvents)
			duration := float64(time.Since(start).Milliseconds())
			healthEventPublishDuration.Observe(duration)
			if err != nil {
				klog.Error(err)
			} else if len(healthEvents.Events) > 0 {
				healthEventsPublished.Add(float64(len(healthEvents.Events)))
			}
		}
	}
}
