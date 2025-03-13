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
	"encoding/csv"
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
	healthEventsInsertionToUDSSucceed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "health_events_insertion_to_uds_succeed",
		Help: "Total number of successful insertion of health events to UDS",
	})

	healthEventsInsertionToUDSError = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "health_events_insertion_to_uds_error",
		Help: "Error in insertions of health events to UDS",
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

func SxidEvent2HealthEvents(sxidEvent *sxid.SXIDErrorEvent, nodeName string,
	recommendationAction pb.RecommenedAction, xidErrorMapping XIDErrorMapping) *pb.HealthEvents {
	healthEvents := pb.HealthEvents{Version: 1, Events: make([]*pb.HealthEvent, 0)}

	event := pb.HealthEvent{
		Version:            1,
		Agent:              AGENT,
		CheckName:          CHECK_NAME,
		ComponentClass:     COMPONENT_CLASS,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		IsFatal:            xidErrorMapping.Fatality == "FATAL",
		Message:            sxidEvent.Message,
		IsHealthy:          sxidEvent.IsHealthy,
		NodeName:           nodeName,
		RecommendedAction:  recommendationAction,
	}
	klog.Infoln("Recommended action: ", event.RecommendedAction)
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
	var sxidErrorMappingConfigFile = flag.String("sxid-error-mapping-config-file",
		"/etc/nvswitchhealthmonitor/sxiderrorsmapping.csv",
		"path to the sxid error mapping config file")

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

	recommendationActionMapping := createRecommendationActionMapping(*configFile)

	xidErrorMapping, err := getXIDErrorMapping(*sxidErrorMappingConfigFile)
	if err != nil {
		klog.Fatalf("failed to get xid error mapping: %v", err)
	}

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
			xidErrorMapping := xidErrorMapping[sxidError.ErrorNum]
			recommendationAction := recommendationActionMapping[xidErrorMapping.RecommendedAction]
			healthEvents := SxidEvent2HealthEvents(sxidError, nodeName, recommendationAction, xidErrorMapping)

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
			healthEventsInsertionToUDSSucceed.Inc()
			healthEventsInsertionToUDSError.Set(0.0)

			if len(healthEvents.Events) > 0 {
				healthEventsPublished.Add(float64(len(healthEvents.Events)))
			}

			return true, nil
		}

		if isRetryableError(err) {
			klog.Errorf("Retryable error occurred: %v", err)
			return false, nil
		}

		klog.Errorf("Non-retryable error occurred: %v", err)

		return false, err
	})

	if err != nil {
		healthEventsInsertionToUDSError.Set(1.0)
		healthEventsPublishFailed.With(prometheus.Labels{"event": fmt.Sprintf("%+v", healthEvents.Events[0])}).Inc()
		klog.Errorf("All retry attempts to send health event failed: %v", err)
	}
}

type XIDErrorMapping struct {
	XIDError          string
	Name              string
	RecommendedAction string
	Fatality          string
}

func getXIDErrorMapping(filePath string) (map[int]XIDErrorMapping, error) {
	errorMappings := make(map[int]XIDErrorMapping)

	csvFile, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sxid error mapping config file: %w", err)
	}

	csvReader := csv.NewReader(csvFile)
	csvReader.FieldsPerRecord = -1
	csvReader.TrimLeadingSpace = true

	records, err := csvReader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failed to read sxid error mapping config file: %w", err)
	}

	for _, record := range records {
		if len(record) != 4 {
			return nil, fmt.Errorf("invalid number of fields in sxid error mapping config file: %w", err)
		}

		xidError, err := strconv.Atoi(record[0])
		if err != nil {
			return nil, fmt.Errorf("failed to convert sxid error to int: %w", err)
		}

		errorMappings[xidError] = XIDErrorMapping{
			XIDError:          record[0],
			Name:              record[1],
			RecommendedAction: record[2],
			Fatality:          record[3],
		}
	}

	return errorMappings, nil
}

func createRecommendationActionMapping(configFile string) map[string]pb.RecommenedAction {
	recommendationActionMapping := make(map[string]pb.RecommenedAction)

	cfg, err := ini.Load(configFile)
	if err != nil {
		klog.Fatalf("failed to load config file: %v", err)
	}

	section := cfg.Section("sxiderrorrecommendactiontoplatformconnectormapping")
	for key, value := range section.KeysHash() {
		valueInt, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			klog.Errorf("failed to convert value to int32: %v", err)
			continue
		}

		recommendationActionMapping[key] = pb.RecommenedAction(valueInt)
	}

	return recommendationActionMapping
}
