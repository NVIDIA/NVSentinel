// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package storewatcher

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

type MongoDBClientTLSCertConfig struct {
	TlsCertPath string `env:"MONGODB_CLIENT_CERT_PATH,required"`
	TlsKeyPath  string `env:"MONGODB_CLIENT_KEY_PATH,required"`
	CaCertPath  string `env:"MONGODB_CA_CERT_PATH,required"`
}

// MongoDBConfig holds the MongoDB connection configuration.
type MongoDBConfig struct {
	URI                 string                     `env:"MONGODB_URI,required"`
	Database            string                     `env:"MONGODB_DATABASE_NAME,required"`
	Collection          string                     `env:"MONGODB_COLLECTION_NAME,required"`
	ClientTLSCertConfig MongoDBClientTLSCertConfig `envPrefix:""`

	TotalPingTimeoutSeconds  int `env:"MONGODB_PING_TIMEOUT_TOTAL_SECONDS" envDefault:"300"` //nolint:lll
	TotalPingIntervalSeconds int `env:"MONGODB_PING_INTERVAL_SECONDS" envDefault:"5"`        //nolint:lll

	TotalCACertTimeoutSeconds  int `env:"CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS" envDefault:"360"` //nolint:lll
	TotalCACertIntervalSeconds int `env:"CA_CERT_READ_INTERVAL_SECONDS" envDefault:"5"`         //nolint:lll

	ChangeStreamRetryDeadlineSeconds int `env:"MONGODB_CHANGE_STREAM_RETRY_DEADLINE_SECONDS" envDefault:"300"` //nolint:lll
	ChangeStreamRetryIntervalSeconds int `env:"MONGODB_CHANGE_STREAM_RETRY_INTERVAL_SECONDS" envDefault:"5"`   //nolint:lll

	UnprocessedEventsMetricUpdateIntervalSeconds int `env:"UNPROCESSED_EVENTS_METRIC_UPDATE_INTERVAL_SECONDS" envDefault:"25"` //nolint:lll

	// AppName is used to identify the client in database connection tracking
	AppName string `env:"APP_NAME"`
}

// TokenConfig holds the token-specific configuration.
type TokenConfig struct {
	ClientName      string
	TokenDatabase   string `env:"MONGODB_DATABASE_NAME,required"`
	TokenCollection string `env:"MONGODB_TOKEN_COLLECTION_NAME,required"`
}

// LoadConfigFromEnv loads both MongoDB and Token configuration from environment variables.
// Returns MongoDBConfig, TokenConfig, and error if any required variables are missing or invalid.
func LoadConfigFromEnv(clientName string) (MongoDBConfig, TokenConfig, error) {
	var mongoConfig MongoDBConfig

	if err := env.Parse(&mongoConfig); err != nil {
		return MongoDBConfig{}, TokenConfig{}, fmt.Errorf("failed to parse MongoDB environment variables: %w", err)
	}

	// Set AppName for MongoDB connection tracking if not already set from environment
	if mongoConfig.AppName == "" {
		mongoConfig.AppName = clientName
	}

	var tokenConfig TokenConfig

	if err := env.Parse(&tokenConfig); err != nil {
		return MongoDBConfig{}, TokenConfig{}, fmt.Errorf("failed to parse token environment variables: %w", err)
	}

	tokenConfig.ClientName = clientName

	return mongoConfig, tokenConfig, nil
}
