// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/klog/v2"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
	fd "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/syslog-monitor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

const (
	defaultAgentName       = "syslog-health-monitor"
	defaultComponentClass  = "GPU" // Or a more specific class if applicable
	defaultPollingInterval = "30m" // Added default polling interval
	defaultStateFilePath   = "/var/run/syslog_monitor/state.json" // Added default state file path
)

// ConfigFile matches the top-level structure of the YAML config file
type ConfigFile struct {
	Checks []common.CheckDefinition `yaml:"checks"`
}

func main() {
	klog.InitFlags(nil)

	configFile := flag.String("config-file", "/etc/config/config.yaml", "Path to the YAML configuration file for log checks.")
	platformConnectorSocket := flag.String("platform-connector-socket", "unix:///var/run/nvsentinel.sock", "Path to the platform-connector UDS socket.")
	nodeNameEnv := flag.String("node-name", os.Getenv("NODE_NAME"), "Node name. Defaults to NODE_NAME env var.")
	pollingIntervalFlag := flag.String("polling-interval", defaultPollingInterval, "Polling interval for health checks (e.g., 15m, 1h).") // Added polling interval flag
	stateFileFlag := flag.String("state-file", defaultStateFilePath, "Path to state file for cursor persistence.") // Added state file flag

	defer klog.Flush()
	flag.Parse()

	nodeName := *nodeNameEnv
	if nodeName == "" {
		klog.Fatalf("NODE_NAME env not set and --node-name flag not provided, cannot run.")
	}
	klog.Infof("Using node name: %s", nodeName)

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))

	conn, err := grpc.NewClient(*platformConnectorSocket, opts...)
	if err != nil {
		klog.Errorf("Error creating gRPC client: %v", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := pb.NewPlatformConnectorClient(conn)

	klog.Infof("Loading checks from config file: %s", *configFile)
	yamlFile, err := os.ReadFile(*configFile)
	if err != nil {
		klog.Errorf("Error reading config file '%s': %v", *configFile, err)
		os.Exit(1)
	}

	var config ConfigFile
	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		klog.Errorf("Error unmarshalling config file '%s': %v", *configFile, err)
		os.Exit(1)
	}

	if len(config.Checks) == 0 {
		klog.Errorln("Error: No checks defined in the config file.")
		os.Exit(1)
	}

	fdHealthMonitor, err := fd.NewSyslogMonitor(nodeName, config.Checks, client, defaultAgentName, defaultComponentClass, *pollingIntervalFlag, *stateFileFlag)
	if err != nil {
		klog.Errorf("Error creating syslog health monitor: %v", err)
		os.Exit(1)
	}

	// Parse polling interval
	pollingInterval, err := time.ParseDuration(*pollingIntervalFlag)
	if err != nil {
		klog.Fatalf("Error parsing polling interval '%s': %v", *pollingIntervalFlag, err)
	}
	klog.Infof("Polling every %v", pollingInterval)

	ticker := time.NewTicker(pollingInterval)
	defer ticker.Stop()

	klog.Infof("config.checks: %v", config.Checks)

	// Polling loop
	for range ticker.C {
		klog.Info("Performing scheduled health check run...")
		if err := fdHealthMonitor.Run(); err != nil {
			klog.Errorf("Error running syslog health monitor: %v", err)
		}
	}

}
