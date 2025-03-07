// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

const (
	AGENT           = "nvswitch-health-monitor"
	CHECK_NAME      = "NvswitchErrorFromKmsgWatch"
	COMPONENT_CLASS = "nvswitch"
)

const defaultStateFilePath = "/var/run/nvswitch_monitor/state.json"

const (
	defaultMaxRetriesForHealthyEvent        = 10
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
	health_events_insertion_to_uds_succeed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "health_events_insertion_to_uds_succeed",
		Help: "Total number of successful insertion of health events to UDS",
	})

	health_events_insertion_to_uds_failed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "health_events_insertion_to_uds_failed",
		Help: "Total number of failed insertion of health events to UDS",
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

func GetEntityIDsForDGXType() (nvswitchIds, nvlinkIds, gpuIds []int, err error) {
	dgxType := lsnvlink.GetDGXType()

	if dgxType == lsnvlink.DGX_TYPE_A100 {
		dgx := lsnvlink.DGX_A100{}
		return dgx.GetAllNVSwitchIds(), dgx.GetAllNVLinkIds(), dgx.GetAllGPUIds(), nil
	} else if dgxType == lsnvlink.DGX_TYPE_H100 {
		dgx := lsnvlink.DGX_H100{}
		return dgx.GetAllNVSwitchIds(), dgx.GetAllNVLinkIds(), dgx.GetAllGPUIds(), nil
	}

	return nil, nil, nil,
		errors.New("failed to get entity ids associated, dgx type is unknown")
}

func SxidEvent2HealthEvents(sxidEvent *sxid.SXIDErrorEvent, nodeName string) *pb.HealthEvents {
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
		NodeName:           nodeName,
	}

	if !sxidEvent.IsHealthy {
		entitiesImpacted := []*pb.Entity{
			{EntityType: "NVSWITCH", EntityValue: strconv.Itoa(sxidEvent.NVSwitch)},
			{EntityType: "PCI", EntityValue: sxidEvent.PCI},
			{EntityType: "NVLINK", EntityValue: strconv.Itoa(sxidEvent.Link)},
		}
		start := time.Now()

		gpuID, err := GetGPUID(sxidEvent.NVSwitch, sxidEvent.Link)

		duration := float64(time.Since(start).Milliseconds())

		if err == nil {
			gpuIdCalculationDuration.With(prometheus.Labels{"gpu_id": fmt.Sprint(gpuID)}).Observe(duration)
			entitiesImpacted = append(entitiesImpacted, &pb.Entity{EntityType: "GPU", EntityValue: strconv.Itoa(gpuID)})
		} else {
			klog.Errorf("Error occurred while computing GPU ID for NVSwitch ID %d and NVLINK ID %d: %v",
				sxidEvent.NVSwitch, sxidEvent.Link, err)
		}

		event.EntitiesImpacted = entitiesImpacted

		event.ErrorCode = []string{fmt.Sprint(sxidEvent.ErrorNum)}
	} else {
		// if this is a healthy event then the node has rebooted, so all
		// entities need to be broadcasted as being healthy initially
		nvswitchIds, nvlinkIds, gpuIds, err := GetEntityIDsForDGXType()
		if err != nil {
			klog.Fatalf("Error occurred while getting entity IDs for DGX type: %v", err)
		}

		entitiesImpacted := []*pb.Entity{}

		for _, id := range nvswitchIds {
			entitiesImpacted = append(
				entitiesImpacted,
				&pb.Entity{EntityType: "NVSWITCH", EntityValue: strconv.Itoa(id)},
			)
		}

		for _, id := range nvlinkIds {
			entitiesImpacted = append(
				entitiesImpacted,
				&pb.Entity{EntityType: "NVLINK", EntityValue: strconv.Itoa(id)},
			)
		}

		for _, id := range gpuIds {
			entitiesImpacted = append(
				entitiesImpacted,
				&pb.Entity{EntityType: "GPU", EntityValue: strconv.Itoa(id)},
			)
		}

		for _, pciAddress := range lsnvlink.GetNVSwitchPCIAddresses() {
			entitiesImpacted = append(
				entitiesImpacted,
				&pb.Entity{EntityType: "PCI", EntityValue: pciAddress},
			)
		}

		event.EntitiesImpacted = entitiesImpacted
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

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		klog.Fatalf("Failed to fetch nodename")
	}

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
			healthEvents := SxidEvent2HealthEvents(sxidError, nodeName)

			retryDelay := time.Duration(nvswitchConfig.RetryDelaySecondsForHealthyEvent) * time.Second
			sendHealthEventWithRetry(client, healthEvents, nvswitchConfig.MaxRetriesForHealthyEvent, retryDelay)
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
	maxRetries int, retryDelay time.Duration) {
	backoff := wait.Backoff{
		Steps:    maxRetries,
		Duration: retryDelay,
		Factor:   2,
		Jitter:   0.1,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		start := time.Now()

		_, err := client.HealthEventOccuredV1(context.Background(), healthEvents)

		duration := float64(time.Since(start).Milliseconds())
		healthEventPublishDuration.Observe(duration)

		if err == nil {
			klog.Infof("Successfully sent health event: %+v", healthEvents)
			health_events_insertion_to_uds_succeed.Inc()

			if len(healthEvents.Events) > 0 {
				healthEventsPublished.Add(float64(len(healthEvents.Events)))
			}

			return true, nil
		}

		health_events_insertion_to_uds_failed.Inc()
		if isRetryableError(err) {
			klog.Errorf("Retryable error occurred: %v", err)
			return false, nil
		}

		klog.Errorf("Non-retryable error occurred: %v", err)

		return false, err
	})

	if err != nil {
		healthEventsPublishFailed.With(prometheus.Labels{"event": fmt.Sprintf("%+v", healthEvents.Events[0])}).Inc()
		klog.Errorf("All retry attempts to send health event failed: %v", err)
	}
}
