package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	nic "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nic-health-monitor/pkg/nic-monitor"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nic-health-monitor/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/ini.v1"
	"k8s.io/klog"
)

const (
	AGENT                      = "nic-health-monitor"
	INFINIBAND_CHECK_NAME      = "InfiniBandErrorCheck"
	INFINIBAND_COMPONENT_CLASS = "infiniBand"

	ETHERNET_CHECK_NAME      = "EthernetErrorCheck"
	ETHERNET_COMPONENT_CLASS = "ethernet"
)

var (
	healthEventsPublished = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nic_monitor_health_events_published_total",
		Help: "The total number of health events that the nic monitor has raised",
	})

	healthEventPublishDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "nic_monitor_health_event_publish_duration_milliseconds",
		Help:    "The time taken by nic monitor to publish health event in milliseconds",
		Buckets: prometheus.LinearBuckets(0, 10, 500),
	})
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

// nolint: gocognit, cyclop
func loadConfig(filePath string) (*nic.NicMonitorConfig, error) {
	cfg, err := ini.Load(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// check if the NicExclusionRegex key exists
	section := cfg.Section("")

	pollingIntervalKey, err := section.GetKey("PollingIntervalInMilliseconds")

	var pollingInterval int

	if err != nil || pollingIntervalKey.String() == "" {
		pollingInterval = 1000 // default to 1000 milliseconds
	} else {
		pollingInterval, err = pollingIntervalKey.Int()
		if err != nil {
			return nil, fmt.Errorf("invalid PollingIntervalInMilliseconds value: %w", err)
		}
	}

	key, err := section.GetKey("NicExclusionRegex")
	if err != nil {
		// nolint:nilerr
		return &nic.NicMonitorConfig{
			PollingIntervalInMilliseconds: pollingInterval,
			ExclusionRegexes:              []string{},
		}, nil
	}

	exclusionRegexes := key.String()
	if exclusionRegexes == "" {
		return &nic.NicMonitorConfig{
			PollingIntervalInMilliseconds: pollingInterval,
			ExclusionRegexes:              []string{},
		}, nil
	}

	filteredExclusionRegexList := []string{}

	for _, regex := range strings.Split(exclusionRegexes, ",") {
		regexTrimmed := strings.TrimSpace(regex)
		if regexTrimmed != "" {
			filteredExclusionRegexList = append(filteredExclusionRegexList, regexTrimmed)
		}
	}

	return &nic.NicMonitorConfig{
		ExclusionRegexes:              filteredExclusionRegexList,
		PollingIntervalInMilliseconds: pollingInterval,
	}, nil
}

func main() {
	var socket = flag.String("socket", "unix:///var/run/nvsentinel.sock", "unix domain socket")

	var configFile = flag.String("config", "/etc/nichealthmonitor/config.ini",
		"path to the nic health monitor config file")

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	flag.Parse()

	nicConfig, err := loadConfig(*configFile)
	if err != nil {
		panic(err)
	}

	klog.Infof("NIC names matching these regexes will be excluded: %v\n", nicConfig.ExclusionRegexes)
	klog.Infof("NIC Monitor will poll every %d milliseconds", nicConfig.PollingIntervalInMilliseconds)

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))

	conn, err := grpc.NewClient(*socket, opts...)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	client := pb.NewPlatformConnectorClient(conn)

	nicErrorMonitor, err := nic.NewNicErrorMonitor(nicConfig)
	if err != nil {
		panic(err)
	}

	errChan := make(chan error, 1)
	go func() {
		errChan <- nicErrorMonitor.Run()
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
		case nicError := <-nicErrorMonitor.EventChan:
			healthEvents := NicError2HealthEvents(nicError)
			start := time.Now()

			_, err := client.HealthEventOccuredV1(context.Background(), healthEvents)

			duration := float64(time.Since(start).Milliseconds())
			healthEventPublishDuration.Observe(duration)

			if err != nil {
				klog.Error(err)
			} else {
				klog.Infof("Successfully sent health events: %+v", healthEvents)

				if len(healthEvents.Events) > 0 {
					healthEventsPublished.Add(float64(len(healthEvents.Events)))
				}
			}
		}
	}
}
