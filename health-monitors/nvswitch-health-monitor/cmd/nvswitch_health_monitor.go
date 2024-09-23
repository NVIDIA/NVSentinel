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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/ini.v1"
	"k8s.io/klog"
)

const (
	AGENT           = "nvswitch-health-monitor"
	CHECK_NAME      = "NvswitchErrorFromKmsgWatch"
	COMPONENT_CLASS = "nvswitch"
)

const defaultStateFilePath = "/var/run/nvswitch_monitor/state.json"

const (
	defaultMaxRetriesForHealthyEvent        = 20
	defaultRetryDelaySecondsForHealthyEvent = 5
)

type NVSwitchMonitorConfig struct {
	*sxid.SxidEventMonitorConfig
	MaxRetriesForHealthyEvent        int
	RetryDelaySecondsForHealthyEvent int
}

// prometheus metrics
var (
	healthEventsPublished = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nvswitch_monitor_health_events_published_total",
		Help: "The total number of health events that the nvswitch monitor has raised",
	})

	healthEventsPublishFailed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nvswitch_monitor_health_events_publish_failed_total",
		Help: "The total number of health events that the nvswitch monitor failed to publish",
	}, []string{"event"})

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

func SxidEvent2HealthEvents(sxidEvent *sxid.SXIDErrorEvent) *pb.HealthEvents {
	healthEvents := pb.HealthEvents{Version: 1, Events: make([]*pb.HealthEvent, 0)}

	event := pb.HealthEvent{
		Version:            1,
		Agent:              AGENT,
		CheckName:          CHECK_NAME,
		ComponentClass:     COMPONENT_CLASS,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		IsFatal:            sxidEvent.IsFatal,
		Message:            sxidEvent.Message,
		IsHealthy:          sxidEvent.IsHealthy,
	}

	if !sxidEvent.IsHealthy {
		entitiesImpacted := []string{
			fmt.Sprintf("nvswitch%d", sxidEvent.NVSwitch),
			sxidEvent.PCI,
			fmt.Sprintf("nvlink%d", sxidEvent.Link),
		}
		start := time.Now()

		gpuID, err := GetGPUID(sxidEvent.NVSwitch, sxidEvent.Link)

		duration := float64(time.Since(start).Milliseconds())

		if err != nil {
			gpuIdCalculationDuration.With(prometheus.Labels{"gpu_id": fmt.Sprint(gpuID)}).Observe(duration)
			entitiesImpacted = append(entitiesImpacted, fmt.Sprintf("gpu%d", gpuID))
		}

		event.EntitiesImpacted = entitiesImpacted

		event.ErrorCode = []string{fmt.Sprint(sxidEvent.ErrorNum)}
	}

	healthEvents.Events = append(healthEvents.Events, &event)

	return &healthEvents
}

func loadConfig(filePath string) (*NVSwitchMonitorConfig, error) {
	cfg, err := ini.Load(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	config := &sxid.SxidEventMonitorConfig{
		PollingIntervalInMilliseconds: 100,
	}

	section := cfg.Section("")

	// load PollingIntervalInMilliseconds key
	pollingIntervalKey, err := section.GetKey("PollingIntervalInMilliseconds")
	if err != nil {
		return nil, fmt.Errorf("PollingIntervalInMilliseconds not found in config file: %w", err)
	}

	pollingIntervalValue, parseErr := pollingIntervalKey.Int()
	if parseErr != nil {
		return nil, fmt.Errorf("invalid PollingIntervalInMilliseconds value: %w", parseErr)
	}

	config.PollingIntervalInMilliseconds = pollingIntervalValue

	maxRetriesForHealthyEvent := defaultMaxRetriesForHealthyEvent
	maxRetriesKey, err := section.GetKey("MaxRetriesForHealthyEvent")
	if err == nil {
		maxRetriesValue, parseErr := maxRetriesKey.Int()
		if parseErr != nil {
			klog.Warningf("Invalid MaxRetriesForHealthyEvent value in config file: %v. Using default: %d",
				parseErr, defaultMaxRetriesForHealthyEvent)
		} else {
			maxRetriesForHealthyEvent = maxRetriesValue
		}
	} else {
		klog.Infof("MaxRetriesForHealthyEvent not found in config file. Using default: %d",
			defaultMaxRetriesForHealthyEvent)
	}

	retryDelaySecondsForHealthyEvent := defaultRetryDelaySecondsForHealthyEvent
	retryDelayKey, err := section.GetKey("RetryDelaySecondsForHealthyEvent")
	if err == nil {
		retryDelayValue, parseErr := retryDelayKey.Int()
		if parseErr != nil {
			klog.Warningf("Invalid RetryDelaySecondsForHealthyEvent value in config file: %v. Using default: %d",
				parseErr, defaultRetryDelaySecondsForHealthyEvent)
		} else {
			retryDelaySecondsForHealthyEvent = retryDelayValue
		}
	} else {
		klog.Infof("RetryDelaySecondsForHealthyEvent not found in config file. Using default: %d",
			defaultRetryDelaySecondsForHealthyEvent)
	}

	return &NVSwitchMonitorConfig{
		SxidEventMonitorConfig:           config,
		MaxRetriesForHealthyEvent:        maxRetriesForHealthyEvent,
		RetryDelaySecondsForHealthyEvent: retryDelaySecondsForHealthyEvent,
	}, nil
}

// nolint:cyclop
func main() {
	var socket = flag.String("socket", "unix:///var/run/nvsentinel.sock", "unix domain socket")

	var configFile = flag.String("config", "/etc/nvswitchhealthmonitor/config.ini",
		"path to the nvswitch health monitor config file")

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	flag.Parse()

	nvswitchConfig, err := loadConfig(*configFile)
	if err != nil {
		panic(err)
	}

	klog.Infof("NVSwitch Monitor will poll every %d milliseconds\n", nvswitchConfig.PollingIntervalInMilliseconds)

	nvswitchConfig.StateFilePath = defaultStateFilePath

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	conn, err := grpc.NewClient(*socket, opts...)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	client := pb.NewPlatformConnectorClient(conn)

	sxidErrorMonitor, err := sxid.NewSxidEventMonitor(nvswitchConfig.SxidEventMonitorConfig)
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
			healthEvents := SxidEvent2HealthEvents(sxidError)

			// we need to retry here because as the node rebooted the platform connectors may take time to come up
			if sxidError.IsHealthy {
				err := sendHealthEventWithRetry(client, healthEvents, nvswitchConfig.MaxRetriesForHealthyEvent,
					time.Duration(nvswitchConfig.RetryDelaySecondsForHealthyEvent)*time.Second)
				if err != nil {
					klog.Error(err)
					healthEventsPublishFailed.With(prometheus.Labels{"event": fmt.Sprintf("%+v", healthEvents.Events[0])}).Inc()
				} else {
					klog.Infof("Successfully sent health event: %+v", healthEvents.Events)
				}
				continue
			}
			start := time.Now()
			_, err := client.HealthEventOccuredV1(context.Background(), healthEvents)
			duration := float64(time.Since(start).Milliseconds())
			healthEventPublishDuration.Observe(duration)
			if err != nil {
				klog.Error(err)
				healthEventsPublishFailed.With(prometheus.Labels{"event": fmt.Sprintf("%+v", healthEvents.Events[0])}).Inc()
			} else if len(healthEvents.Events) > 0 {
				healthEventsPublished.Add(float64(len(healthEvents.Events)))
				klog.Infof("Successfully sent health event: %+v", healthEvents.Events)
			}
		}
	}
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	if s, ok := status.FromError(err); ok {
		if s.Code() == codes.Unavailable {
			return true
		}
	}

	return false
}

func sendHealthEventWithRetry(client pb.PlatformConnectorClient, healthEvents *pb.HealthEvents,
	maxRetries int, retryDelay time.Duration) error {
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		start := time.Now()

		_, err = client.HealthEventOccuredV1(context.Background(), healthEvents)

		duration := float64(time.Since(start).Milliseconds())
		healthEventPublishDuration.Observe(duration)

		if err == nil {
			healthEventsPublished.Add(float64(len(healthEvents.Events)))
			return nil
		}

		if isRetryableError(err) {
			klog.Errorf("Attempt %d/%d: Failed to send health event due to retryable error: %v", attempt, maxRetries, err)

			if attempt < maxRetries {
				time.Sleep(retryDelay)
			}
		} else {
			// non-retryable error encountered, log and exit
			klog.Errorf("Failed to send health event due to non-retryable error: %v", err)
			break
		}
	}

	klog.Error("All retry attempts to send health event failed.")
	return err
}
