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
	"path/filepath"
	"strconv"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-notification-module/pkg/config"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-notification-module/pkg/reconciler"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/store-client-sdk/pkg/storewatcher"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog"
)

func main() {
	ctx := context.Background()

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	var mongoClientCertMountPath = flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	var kubeconfigPath = flag.String("kubeconfig-path", "", "path to kubeconfig file")

	var tomlConfigPath = flag.String("config-path", "/etc/config/config.toml",
		"path where the fault notification config file is present")

	flag.Parse()

	klog.Infof("Mongo client cert path taken is: %s", *mongoClientCertMountPath)
	mongoURI := os.Getenv("MONGODB_URI")
	if mongoURI == "" {
		klog.Fatalf("MongoDB URI is not provided")
	}

	mongoDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if mongoDatabase == "" {
		klog.Fatalf("MongoDB Database name is not provided")
	}

	mongoCollection := os.Getenv("MONGODB_COLLECTION_NAME")
	if mongoCollection == "" {
		klog.Fatalf("MongoDB collection name is not provided")
	}

	tokenDatabase := os.Getenv("MONGODB_DATABASE_NAME")
	if tokenDatabase == "" {
		klog.Fatalf("MongoDB token database name is not provided")
	}

	tokenCollection := os.Getenv("MONGODB_TOKEN_COLLECTION_NAME")
	if tokenCollection == "" {
		klog.Fatalf("MongoDB token collection name is not provided")
	}

	totalTimeoutSeconds, err := getEnvAsInt("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	if err != nil {
		klog.Fatalf("invalid MONGODB_PING_TIMEOUT_TOTAL_SECONDS: %v", err)
	}

	intervalSeconds, err := getEnvAsInt("MONGODB_PING_INTERVAL_SECONDS", 5)
	if err != nil {
		klog.Fatalf("invalid MONGODB_PING_INTERVAL_SECONDS: %v", err)
	}

	totalCACertTimeoutSeconds, err := getEnvAsInt("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	if err != nil {
		klog.Fatalf("invalid CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS: %v", err)
	}

	intervalCACertSeconds, err := getEnvAsInt("CA_CERT_READ_INTERVAL_SECONDS", 5)
	if err != nil {
		klog.Fatalf("invalid CA_CERT_READ_INTERVAL_SECONDS: %v", err)
	}

	klog.Infof("Starting a metrics port on : %s", *metricsPort)
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		err := http.ListenAndServe(":"+*metricsPort, nil)
		if err != nil {
			klog.Fatalf("Failed to start metrics server: %v", err)
		}
	}()

	mongoConfig := storewatcher.MongoDBConfig{
		URI:        mongoURI,
		Database:   mongoDatabase,
		Collection: mongoCollection,
		ClientTLSCertConfig: storewatcher.MongoDBClientTLSCertConfig{
			TlsCertPath: filepath.Join(*mongoClientCertMountPath, "tls.crt"),
			TlsKeyPath:  filepath.Join(*mongoClientCertMountPath, "tls.key"),
			CaCertPath:  filepath.Join(*mongoClientCertMountPath, "ca.crt"),
		},
		TotalPingTimeoutSeconds:    totalTimeoutSeconds,
		TotalPingIntervalSeconds:   intervalSeconds,
		TotalCACertTimeoutSeconds:  totalCACertTimeoutSeconds,
		TotalCACertIntervalSeconds: intervalCACertSeconds,
	}

	tokenConfig := storewatcher.TokenConfig{
		ClientName:      "fault-notification-module",
		TokenDatabase:   tokenDatabase,
		TokenCollection: tokenCollection,
	}

	pipeline := mongo.Pipeline{
		{
			{Key: "$match", Value: bson.D{
				{Key: "operationType", Value: "update"},
				{Key: "$or", Value: bson.A{
					bson.D{{Key: "updateDescription.updatedFields", Value: bson.D{{Key: "healtheventstatus.nodequarantined", Value: true}}}},
					bson.D{{Key: "updateDescription.updatedFields", Value: bson.D{{Key: "healtheventstatus.nodequarantined", Value: false}}}},
				}},
			}},
		},
	}

	tomlCfg, err := config.LoadTomlConfig(*tomlConfigPath)
	if err != nil {
		klog.Fatalf("error while loading the toml config: %v", err)
	}

	// Initialize the k8s client
	k8sClient, err := reconciler.NewFaultNotificationClient(*kubeconfigPath)
	if err != nil {
		klog.Fatalf("error while initializing kubernetes client: %v", err)
	}

	klog.Info("Successfully initialized k8sclient")

	reconcilerCfg := reconciler.ReconcilerConfig{
		TomlConfig:    *tomlCfg,
		MongoConfig:   mongoConfig,
		TokenConfig:   tokenConfig,
		MongoPipeline: pipeline,
		K8sClient:     k8sClient,
	}

	reconciler := reconciler.NewReconciler(reconcilerCfg)
	reconciler.Start(ctx)
}

func getEnvAsInt(name string, defaultValue int) (int, error) {
	valueStr, exists := os.LookupEnv(name)
	if !exists {
		return defaultValue, nil
	}

	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return 0, fmt.Errorf("error converting %s to integer: %w", name, err)
	}

	if value <= 0 {
		return 0, fmt.Errorf("value of %s must be a positive integer", name)
	}

	return value, nil
}
