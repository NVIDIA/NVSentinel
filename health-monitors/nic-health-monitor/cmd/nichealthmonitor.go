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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	nic "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nic-health-monitor/pkg/nic-monitor"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nic-health-monitor/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/ini.v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
)

const (
	AGENT                 = "nic-health-monitor"
	INFINIBAND_CHECK_NAME = "InfiniBandErrorCheck"

	ETHERNET_CHECK_NAME = "EthernetErrorCheck"
	COMPONENT_CLASS     = "NIC"
	UNKNOWN_LINK_LAYER  = "unknown"

	DEFAULT_SYS_CLASS_NET_PATH        = "/sys/class/net"
	DEFAULT_SYS_CLASS_INFINIBAND_PATH = "/sys/class/infiniband"
)

const defaultStateFilePath = "/var/run/nic_monitor/state.json"

const (
	defaultPollingIntervalInMilliseconds                    = 1000
	defaultMaxRetryDurationForDownDetectedNICInMilliseconds = 500
	defaultRetryIntervalForDownDetectedNICInMilliseconds    = 100
	defaultMaxRetriesForRetryableError                      = 10
	defaultRetryDelaySecondsForRetryableError               = 5
	defaultMonitorNetworkType                               = string(nic.MonitorNetworkTypeAll)
	defaultRoCEInterfaceRegexes                             = "^rdma\\d+$,^eth\\d+$"
)

// connectionManager manages the gRPC connection and handles reconnection
type connectionManager struct {
	socket string
	opts   []grpc.DialOption
	conn   *grpc.ClientConn
	client pb.PlatformConnectorClient
	mu     sync.Mutex
}

func newConnectionManager(socket string, opts []grpc.DialOption) (*connectionManager, error) {
	cm := &connectionManager{
		socket: socket,
		opts:   opts,
	}
	if err := cm.connect(); err != nil {
		return nil, err
	}

	return cm, nil
}

func (cm *connectionManager) connect() error {
	conn, err := grpc.NewClient(cm.socket, cm.opts...)
	if err != nil {
		return err
	}

	cm.conn = conn
	cm.client = pb.NewPlatformConnectorClient(conn)

	return nil
}

func (cm *connectionManager) getClient() (pb.PlatformConnectorClient, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// Check connection state
	if cm.conn == nil || cm.conn.GetState() == connectivity.Shutdown ||
		cm.conn.GetState() == connectivity.TransientFailure {
		// Close old connection if exists
		if cm.conn != nil {
			cm.conn.Close()
		}
		// Reconnect
		if err := cm.connect(); err != nil {
			return nil, err
		}

		klog.Info("Reconnected to gRPC server")
	}

	return cm.client, nil
}

func (cm *connectionManager) close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.conn != nil {
		cm.conn.Close()
	}
}

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

	healthEventsInsertionToUDSSucceed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "health_events_insertion_to_uds_succeed",
		Help: "Total number of successful insertion of health events to UDS",
	})

	healthEventsInsertionToUDSError = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "health_events_insertion_to_uds_error",
		Help: "Error in insertions of health events to UDS",
	})
)

func NicEvent2HealthEvents(nicEvents *[]nic.NicHealthEvent, nodeName string) *pb.HealthEvents {
	healthEvents := pb.HealthEvents{Version: 1, Events: make([]*pb.HealthEvent, 0)}

	for _, nicEvent := range *nicEvents {
		var checkname, componentClass string
		componentClass = COMPONENT_CLASS

		// Determine check name based on link layer instead of NicType
		// Treat "unknown" link layer as Ethernet
		switch nicEvent.LinkLayer {
		case "InfiniBand":
			checkname = INFINIBAND_CHECK_NAME
		case "Ethernet", nic.UNKNOWN_LINK_LAYER, "":
			checkname = ETHERNET_CHECK_NAME
		default:
			// Fallback to original logic for backward compatibility
			switch nicEvent.NicType {
			case nic.Infiniband:
				checkname = INFINIBAND_CHECK_NAME
			case nic.Ethernet:
				checkname = ETHERNET_CHECK_NAME
			}
		}

		isHealthy := nicEvent.IsHealthyEvent
		isFatal := !isHealthy

		event := pb.HealthEvent{
			Version:            1,
			Agent:              AGENT,
			CheckName:          checkname,
			ComponentClass:     componentClass,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			Message:            nicEvent.Message,
			IsFatal:            isFatal,
			IsHealthy:          isHealthy,
			NodeName:           nodeName,
			RecommendedAction:  pb.RecommenedAction_NONE,
		}

		if nicEvent.Name != "" {
			event.EntitiesImpacted = []*pb.Entity{{EntityType: "NIC", EntityValue: nicEvent.Name}}
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

	getStringValue := func(keyName string, defaultVal string) string {
		key := section.Key(keyName)
		if key == nil || key.String() == "" {
			return defaultVal
		}

		return key.String()
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

	maxRetriesForRetryableError, err := getIntValue("MaxRetriesForRetryableError",
		defaultMaxRetriesForRetryableError)
	if err != nil {
		return nil, err
	}

	retryDelaySecondsForRetryableError, err := getIntValue("RetryDelaySecondsForRetryableError",
		defaultRetryDelaySecondsForRetryableError)
	if err != nil {
		return nil, err
	}

	monitorNetworkType := getStringValue("MonitorNetworkType", defaultMonitorNetworkType)

	// Read SysClassNetPath from configmap, default to /sys/class/net if not specified
	sysClassNetPath := section.Key("SysClassNetPath").String()
	if sysClassNetPath == "" {
		sysClassNetPath = DEFAULT_SYS_CLASS_NET_PATH
	}

	// Read SysClassInfinibandPath from configmap, default to /sys/class/infiniband if not specified
	sysClassInfinibandPath := section.Key("SysClassInfinibandPath").String()
	if sysClassInfinibandPath == "" {
		sysClassInfinibandPath = DEFAULT_SYS_CLASS_INFINIBAND_PATH
	}

	return &nic.NicMonitorConfig{
		PollingIntervalInMilliseconds:                    pollingInterval,
		ExclusionRegexes:                                 exclusionRegexes,
		MaxRetryDurationForDownDetectedNICInMilliseconds: maxRetryDuration,
		RetryIntervalForDownDetectedNICInMilliseconds:    retryInterval,
		MaxRetriesForRetryableError:                      maxRetriesForRetryableError,
		RetryDelaySecondsForRetryableError:               retryDelaySecondsForRetryableError,
		MonitorNetworkType:                               nic.MonitorNetworkType(monitorNetworkType),
		SysClassNetPath:                                  sysClassNetPath,
		SysClassInfinibandPath:                           sysClassInfinibandPath,
	}, nil
}

//nolint:cyclop
func validateConfig(cfg *nic.NicMonitorConfig) error {
	if cfg.PollingIntervalInMilliseconds <= 0 {
		return fmt.Errorf("PollingIntervalInMilliseconds must be a positive integer")
	}

	if cfg.MaxRetryDurationForDownDetectedNICInMilliseconds <= 0 {
		return fmt.Errorf("MaxRetryDurationForDownDetectedNICInMilliseconds must be a positive integer")
	}

	if cfg.MaxRetryDurationForDownDetectedNICInMilliseconds >= cfg.PollingIntervalInMilliseconds {
		return fmt.Errorf("MaxRetryDurationForDownDetectedNICInMilliseconds should be strictly less than " +
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

	if cfg.MaxRetriesForRetryableError < 1 {
		return fmt.Errorf("MaxRetriesForRetryableError must not be less than 1")
	}

	if cfg.RetryDelaySecondsForRetryableError <= 0 {
		return fmt.Errorf("RetryDelaySecondsForRetryableError must be a positive integer")
	}

	if cfg.MonitorNetworkType != nic.MonitorNetworkTypeAll && cfg.MonitorNetworkType != nic.MonitorNetworkTypeRoCE &&
		cfg.MonitorNetworkType != nic.MonitorNetworkTypeInfiniBand {
		return fmt.Errorf(
			"invalid MonitorNetworkType: %s. Must be one of '%s', '%s', or '%s'",
			cfg.MonitorNetworkType,
			nic.MonitorNetworkTypeAll,
			nic.MonitorNetworkTypeRoCE,
			nic.MonitorNetworkTypeInfiniBand,
		)
	}

	for _, regex := range cfg.ExclusionRegexes {
		if _, err := regexp.Compile(regex); err != nil {
			return fmt.Errorf("invalid NIC exclusion regex '%s': %w", regex, err)
		}
	}

	for _, regex := range cfg.RoCEInterfaceRegexes {
		if _, err := regexp.Compile(regex); err != nil {
			return fmt.Errorf("invalid RoCE interface regex '%s': %w", regex, err)
		}
	}

	return nil
}

// nolint: cyclop, gocognit
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

		retryIntervalForDownDetectedNIC = flag.Int(
			"retry-interval-for-down-detected-nic",
			defaultRetryIntervalForDownDetectedNICInMilliseconds,
			"Retry interval for down-detected NICs in milliseconds",
		)

		monitorNetworkTypeFlag = flag.String("monitor-network-type", defaultMonitorNetworkType,
			fmt.Sprintf("Type of network to monitor. Options: %s, %s, %s",
				nic.MonitorNetworkTypeAll, nic.MonitorNetworkTypeRoCE, nic.MonitorNetworkTypeInfiniBand))

		roCEInterfaceRegexesFlag = flag.String(
			"roce-interface-regexes",
			defaultRoCEInterfaceRegexes,
			"Comma-separated list of regex patterns for filtering RoCE interfaces in "+
				"/sys/class/infiniband/<device>/device/net (default matches rdma0, rdma1, eth0, eth1, etc.)",
		)
	)

	flag.Parse()

	// initialize config with flag values
	nicConfig := &nic.NicMonitorConfig{
		PollingIntervalInMilliseconds:                    *pollingInterval,
		ExclusionRegexes:                                 []string{},
		MaxRetryDurationForDownDetectedNICInMilliseconds: *maxRetryDurationForDownDetectedNIC,
		RetryIntervalForDownDetectedNICInMilliseconds:    *retryIntervalForDownDetectedNIC,
		MaxRetriesForRetryableError:                      defaultMaxRetriesForRetryableError,
		RetryDelaySecondsForRetryableError:               defaultRetryDelaySecondsForRetryableError,
		StateFilePath:                                    defaultStateFilePath,
		MonitorNetworkType:                               nic.MonitorNetworkType(*monitorNetworkTypeFlag),
		SysClassNetPath:                                  DEFAULT_SYS_CLASS_NET_PATH,
		SysClassInfinibandPath:                           DEFAULT_SYS_CLASS_INFINIBAND_PATH,
	}

	if *nicExclusionRegexes != "" {
		for _, regex := range strings.Split(*nicExclusionRegexes, ",") {
			trimmedRegex := strings.TrimSpace(regex)
			if trimmedRegex != "" {
				nicConfig.ExclusionRegexes = append(nicConfig.ExclusionRegexes, trimmedRegex)
			}
		}
	}

	if *roCEInterfaceRegexesFlag != "" {
		for _, regex := range strings.Split(*roCEInterfaceRegexesFlag, ",") {
			trimmedRegex := strings.TrimSpace(regex)
			if trimmedRegex != "" {
				nicConfig.RoCEInterfaceRegexes = append(nicConfig.RoCEInterfaceRegexes, trimmedRegex)
			}
		}
	} else {
		// Use default if not specified
		nicConfig.RoCEInterfaceRegexes = append(nicConfig.RoCEInterfaceRegexes, defaultRoCEInterfaceRegexes)
	}

	// check if config.ini exists and load it to override flag values
	configFilePath := "/etc/nichealthmonitor/config.ini"
	_, err := os.Stat(configFilePath)

	//nolint
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

		//nolint
		nicConfig.MaxRetryDurationForDownDetectedNICInMilliseconds = fileConfig.MaxRetryDurationForDownDetectedNICInMilliseconds

		nicConfig.RetryIntervalForDownDetectedNICInMilliseconds = fileConfig.RetryIntervalForDownDetectedNICInMilliseconds

		nicConfig.MaxRetriesForRetryableError = fileConfig.MaxRetriesForRetryableError

		nicConfig.RetryDelaySecondsForRetryableError = fileConfig.RetryDelaySecondsForRetryableError

		// MonitorNetworkType from config file takes precedence
		if fileConfig.MonitorNetworkType != "" {
			nicConfig.MonitorNetworkType = fileConfig.MonitorNetworkType
		}

		nicConfig.SysClassNetPath = fileConfig.SysClassNetPath

		nicConfig.SysClassInfinibandPath = fileConfig.SysClassInfinibandPath

		klog.Info("Loaded configuration from configmap")
	}

	if err := validateConfig(nicConfig); err != nil {
		klog.Fatalf("configuration validation failed: %v", err)
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		klog.Fatalf("Failed to fetch nodename")
	}

	klog.Infof("NIC names matching these regexes will be excluded: %v", nicConfig.ExclusionRegexes)
	klog.Infof("NIC Monitor will poll every %d milliseconds", nicConfig.PollingIntervalInMilliseconds)
	klog.Infof("Max Retry Duration for Down-Detected NIC: %d milliseconds",
		nicConfig.MaxRetryDurationForDownDetectedNICInMilliseconds)
	klog.Infof("Retry Interval for Down-Detected NIC: %d milliseconds",
		nicConfig.RetryIntervalForDownDetectedNICInMilliseconds)
	klog.Infof("Monitor Network Type: %s", nicConfig.MonitorNetworkType)

	klog.Infof("Ethernet interfaces will be monitored from path: %s", nicConfig.SysClassNetPath)
	klog.Infof("Infiniband interfaces will be monitored from path: %s", nicConfig.SysClassInfinibandPath)

	if nicConfig.MonitorNetworkType == nic.MonitorNetworkTypeRoCE {
		klog.Infof("RoCE Interface Regexes: %v", nicConfig.RoCEInterfaceRegexes)
	}

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	opts = append(opts, grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                30 * time.Second, // Send pings every 30 seconds
		Timeout:             10 * time.Second, // Wait 10 seconds for ping ack
		PermitWithoutStream: true,             // Send pings even without active streams to keep connection alive
	}))

	connMgr, err := newConnectionManager(*socket, opts)
	if err != nil {
		panic(err)
	}
	defer connMgr.close()

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
			healthEvents := NicEvent2HealthEvents(nicEvent, nodeName)
			if len(healthEvents.Events) == 0 {
				continue
			}

			sendHealthEventWithRetry(connMgr, healthEvents, nicConfig.MaxRetriesForRetryableError,
				time.Duration(nicConfig.RetryDelaySecondsForRetryableError)*time.Second)
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

func sendHealthEventWithRetry(connMgr *connectionManager, healthEvents *pb.HealthEvents,
	maxRetries int, retryDelay time.Duration,
) {
	backoff := wait.Backoff{
		Steps:    maxRetries,
		Duration: retryDelay,
		Factor:   1.5,
		Jitter:   0.1,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		start := time.Now()

		client, err := connMgr.getClient()
		if err != nil {
			klog.Errorf("Failed to get gRPC client: %v", err)
			return false, nil // Retry
		}

		_, err = client.HealthEventOccuredV1(context.Background(), healthEvents)

		duration := float64(time.Since(start).Milliseconds())
		healthEventPublishDuration.Observe(duration)

		if err == nil {
			klog.Infof("Successfully sent health events: %+v", healthEvents)
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
		klog.Errorf("All retry attempts to send health event failed: %v", err)
	}
}
