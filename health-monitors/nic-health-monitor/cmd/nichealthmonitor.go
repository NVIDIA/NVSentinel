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
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
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
	AGENT                 = "nic-health-monitor"
	INFINIBAND_CHECK_NAME = "InfiniBandErrorCheck"

	ETHERNET_CHECK_NAME = "EthernetErrorCheck"
	COMPONENT_CLASS     = "NIC"
)

const (
	defaultPollingIntervalInMilliseconds                    = 1000
	defaultMaxRetryDurationForDownDetectedNICInMilliseconds = 500
	defaultRetryIntervalForDownDetectedNICInMilliseconds    = 100
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

func NicEvent2HealthEvents(nicEvents *[]nic.NicHealthEvent) *pb.HealthEvents {
	healthEvents := pb.HealthEvents{Version: 1, Events: make([]*pb.HealthEvent, 0)}

	for _, nicEvent := range *nicEvents {
		var checkname, componentClass string
		componentClass = COMPONENT_CLASS

		if nicEvent.NicType == nic.Infiniband {
			checkname = INFINIBAND_CHECK_NAME
		} else if nicEvent.NicType == nic.Ethernet {
			checkname = ETHERNET_CHECK_NAME
		}

		isHealthy := nicEvent.IsHealthyEvent
		isFatal := !isHealthy

		event := pb.HealthEvent{
			Version:            1,
			Agent:              AGENT,
			CheckName:          checkname,
			ComponentClass:     componentClass,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			EntitiesImpacted:   []*pb.Entity{{EntityType: "NIC", EntityValue: nicEvent.Name}},
			Message:            nicEvent.Message,
			IsFatal:            isFatal,
			IsHealthy:          isHealthy,
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

	section := cfg.Section("")

	getIntValue := func(keyName string, defaultVal int) (int, error) {
		key := section.Key(keyName)
		if key == nil || key.String() == "" {
			return defaultVal, nil
		}

		value, err := key.Int()
		if err != nil {
			return 0, fmt.Errorf("invalid %s value: %w", keyName, err)
		}

		return value, nil
	}

	pollingInterval, err := getIntValue("PollingIntervalInMilliseconds", defaultPollingIntervalInMilliseconds)
	if err != nil {
		return nil, err
	}

	var exclusionRegexes []string

	key := section.Key("NicExclusionRegex")
	if key != nil && key.String() != "" {
		for _, regex := range strings.Split(key.String(), ",") {
			trimmedRegex := strings.TrimSpace(regex)
			if trimmedRegex != "" {
				exclusionRegexes = append(exclusionRegexes, trimmedRegex)
			}
		}
	}

	maxRetryDuration, err := getIntValue("MaxRetryDurationForDownDetectedNICInMilliseconds",
		defaultMaxRetryDurationForDownDetectedNICInMilliseconds)
	if err != nil {
		return nil, err
	}

	retryInterval, err := getIntValue("RetryIntervalForDownDetectedNICInMilliseconds",
		defaultRetryIntervalForDownDetectedNICInMilliseconds)
	if err != nil {
		return nil, err
	}

	return &nic.NicMonitorConfig{
		PollingIntervalInMilliseconds:                    pollingInterval,
		ExclusionRegexes:                                 exclusionRegexes,
		MaxRetryDurationForDownDetectedNICInMilliseconds: maxRetryDuration,
		RetryIntervalForDownDetectedNICInMilliseconds:    retryInterval,
	}, nil
}

func validateConfig(cfg *nic.NicMonitorConfig) error {
	if cfg.PollingIntervalInMilliseconds <= 0 {
		return fmt.Errorf("PollingIntervalInMilliseconds must be a positive integer")
	}

	if cfg.MaxRetryDurationForDownDetectedNICInMilliseconds <= 0 {
		return fmt.Errorf("MaxRetryDurationForDownDetectedNICInMilliseconds must be a positive integer")
	}

	if cfg.MaxRetryDurationForDownDetectedNICInMilliseconds >= cfg.PollingIntervalInMilliseconds {
		return fmt.Errorf("MaxRetryDurationForDownDetectedNICInMilliseconds should be strictly less than" +
			"PollingIntervalInMilliseconds")
	}

	if cfg.RetryIntervalForDownDetectedNICInMilliseconds <= 0 {
		return fmt.Errorf("RetryIntervalForDownDetectedNICInMilliseconds must be a positive integer")
	}

	if cfg.RetryIntervalForDownDetectedNICInMilliseconds >= cfg.MaxRetryDurationForDownDetectedNICInMilliseconds {
		return fmt.Errorf(
			"RetryIntervalForDownDetectedNICInMilliseconds (%d) must be strictly less than "+
				"MaxRetryDurationForDownDetectedNICInMilliseconds (%d)",
			cfg.RetryIntervalForDownDetectedNICInMilliseconds,
			cfg.MaxRetryDurationForDownDetectedNICInMilliseconds,
		)
	}

	for _, regex := range cfg.ExclusionRegexes {
		if _, err := regexp.Compile(regex); err != nil {
			return fmt.Errorf("invalid NIC exclusion regex '%s': %w", regex, err)
		}
	}

	return nil
}

// nolint: cyclop
func main() {
	var (
		socket = flag.String("socket", "unix:///var/run/nvsentinel.sock", "unix domain socket")

		metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

		pollingInterval = flag.Int("polling-interval", defaultPollingIntervalInMilliseconds,
			"Polling interval in milliseconds")

		nicExclusionRegexes = flag.String("nic-exclusion-regexes", "", "Comma-separated list of NIC exclusion regexes")

		maxRetryDurationForDownDetectedNIC = flag.Int("max-retry-duration-for-down-detected-nic",
			defaultMaxRetryDurationForDownDetectedNICInMilliseconds,
			"Maximum retry duration for down-detected NICs in milliseconds")

		retryIntervalForDownDetectedNIC = flag.Int("retry-interval-for-down-detected-nic",
			defaultRetryIntervalForDownDetectedNICInMilliseconds, "Retry interval for down-detected NICs in milliseconds")
	)

	flag.Parse()

	// initialize config with flag values
	nicConfig := &nic.NicMonitorConfig{
		PollingIntervalInMilliseconds:                    *pollingInterval,
		ExclusionRegexes:                                 []string{},
		MaxRetryDurationForDownDetectedNICInMilliseconds: *maxRetryDurationForDownDetectedNIC,
		RetryIntervalForDownDetectedNICInMilliseconds:    *retryIntervalForDownDetectedNIC,
	}

	if *nicExclusionRegexes != "" {
		for _, regex := range strings.Split(*nicExclusionRegexes, ",") {
			trimmedRegex := strings.TrimSpace(regex)
			if trimmedRegex != "" {
				nicConfig.ExclusionRegexes = append(nicConfig.ExclusionRegexes, trimmedRegex)
			}
		}
	}

	// check if config.ini exists and load it to override flag values
	configFilePath := "/etc/nichealthmonitor/config.ini"
	_, err := os.Stat(configFilePath)

	if err != nil {
		if !os.IsNotExist(err) {
			klog.Fatalf("failed to read config file path: %v", err)
		}

		klog.Info("Loaded configuration from command line flags")
	} else {
		fileConfig, err := loadConfig(configFilePath)
		if err != nil {
			klog.Fatalf("failed to load config from file: %v", err)
		}

		nicConfig.PollingIntervalInMilliseconds = fileConfig.PollingIntervalInMilliseconds

		nicConfig.ExclusionRegexes = fileConfig.ExclusionRegexes

		nicConfig.MaxRetryDurationForDownDetectedNICInMilliseconds =
			fileConfig.MaxRetryDurationForDownDetectedNICInMilliseconds

		nicConfig.RetryIntervalForDownDetectedNICInMilliseconds =
			fileConfig.RetryIntervalForDownDetectedNICInMilliseconds

		klog.Info("Loaded configuration from configmap")
	}

	if err := validateConfig(nicConfig); err != nil {
		klog.Fatalf("configuration validation failed: %v", err)
	}

	klog.Infof("NIC names matching these regexes will be excluded: %v", nicConfig.ExclusionRegexes)
	klog.Infof("NIC Monitor will poll every %d milliseconds", nicConfig.PollingIntervalInMilliseconds)
	klog.Infof("Max Retry Duration for Down-Detected NIC: %d milliseconds",
		nicConfig.MaxRetryDurationForDownDetectedNICInMilliseconds)
	klog.Infof("Retry Interval for Down-Detected NIC: %d milliseconds",
		nicConfig.RetryIntervalForDownDetectedNICInMilliseconds)

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))

	conn, err := grpc.NewClient(*socket, opts...)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	client := pb.NewPlatformConnectorClient(conn)

	nicHealthMonitor, err := nic.NewNicHealthMonitor(nicConfig)
	if err != nil {
		panic(err)
	}

	errChan := make(chan error, 1)
	go func() {
		errChan <- nicHealthMonitor.Run()
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
		case nicEvent := <-nicHealthMonitor.EventChan:
			healthEvents := NicEvent2HealthEvents(nicEvent)
			if len(healthEvents.Events) == 0 {
				continue
			}

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
