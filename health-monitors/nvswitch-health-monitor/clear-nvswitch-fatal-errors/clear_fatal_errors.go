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
	"log"
	"os"
	"strings"
	"time"

	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nvswitch-health-monitor/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	agentName      = "nvswitch-health-monitor"
	componentClass = "nvswitch"
	checkName      = "NvswitchErrorFromKmsgWatch"
	healthyMessage = "No Health Failures"
	udsSocket      = "unix:///var/run/nvsentinel.sock"
)

func parseList(list string) []string {
	if list == "" {
		return nil
	}
	parts := strings.Split(list, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func run() error {
	nvSwitchesFlag := flag.String("nvswitches", "", "Comma-separated NVSwitch IDs (e.g., 0,1,2)")
	nvLinksFlag := flag.String("nvlinks", "", "Comma-separated NVLink IDs (e.g., 04,28)")
	gpusFlag := flag.String("gpus", "", "Comma-separated GPU IDs (e.g., 0,3,7)")
	pcisFlag := flag.String("pcis", "", "Comma-separated PCI addresses (e.g., 0000:06:00.0,0000:c3:00.0)")
	flag.Parse()

	entities := []*pb.Entity{}

	for _, id := range parseList(*nvSwitchesFlag) {
		entities = append(entities, &pb.Entity{EntityType: "NVSWITCH", EntityValue: id})
	}
	for _, id := range parseList(*nvLinksFlag) {
		entities = append(entities, &pb.Entity{EntityType: "NVLINK", EntityValue: id})
	}
	for _, id := range parseList(*gpusFlag) {
		entities = append(entities, &pb.Entity{EntityType: "GPU", EntityValue: id})
	}
	for _, id := range parseList(*pcisFlag) {
		entities = append(entities, &pb.Entity{EntityType: "PCI", EntityValue: id})
	}

	if len(entities) == 0 {
		return fmt.Errorf("no entities provided; use --nvswitches, --nvlinks, --gpus, or --pcis to specify entities to clear")
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return fmt.Errorf("node name is empty")
	}

	he := &pb.HealthEvent{
		NodeName:           nodeName,
		Version:            1,
		Agent:              agentName,
		ComponentClass:     componentClass,
		CheckName:          checkName,
		IsFatal:            false,
		IsHealthy:          true,
		Message:            healthyMessage,
		RecommendedAction:  pb.RecommenedAction_NONE,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   entities,
	}

	events := &pb.HealthEvents{Version: 1, Events: []*pb.HealthEvent{he}}

	log.Printf("Sending healthy event for %d entities", len(entities))

	conn, err := grpc.NewClient(udsSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("gRPC dial error: %w", err)
	}
	defer conn.Close()

	client := pb.NewPlatformConnectorClient(conn)

	if _, err := client.HealthEventOccuredV1(context.Background(), events); err != nil {
		return fmt.Errorf("failed to send HealthEvent: %w", err)
	}

	log.Println("Healthy event sent successfully")
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("%v", err)
	}
}
