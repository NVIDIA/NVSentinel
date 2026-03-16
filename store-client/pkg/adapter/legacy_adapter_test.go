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

package adapter

import (
	"testing"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestConvertDataStoreConfigToLegacy_MongoDBNoTLSReturnsEmptyCertPaths(t *testing.T) {
	dsConfig := &datastore.DataStoreConfig{
		Provider: datastore.ProviderMongoDB,
	}

	certConfig := ConvertDataStoreConfigToLegacy(dsConfig).GetCertConfig()

	if certConfig.GetCertPath() != "" {
		t.Fatalf("expected empty cert path when MongoDB TLS is disabled, got %q", certConfig.GetCertPath())
	}

	if certConfig.GetKeyPath() != "" {
		t.Fatalf("expected empty key path when MongoDB TLS is disabled, got %q", certConfig.GetKeyPath())
	}

	if certConfig.GetCACertPath() != "" {
		t.Fatalf("expected empty CA path when MongoDB TLS is disabled, got %q", certConfig.GetCACertPath())
	}
}

func TestConvertDataStoreConfigToLegacy_MongoDBTLSConfigUsesConfiguredPaths(t *testing.T) {
	dsConfig := &datastore.DataStoreConfig{
		Provider: datastore.ProviderMongoDB,
		Connection: datastore.ConnectionConfig{
			TLSConfig: &datastore.TLSConfig{
				CertPath: "/tmp/mongo/tls.crt",
				KeyPath:  "/tmp/mongo/tls.key",
				CAPath:   "/tmp/mongo/ca.crt",
			},
		},
	}

	certConfig := ConvertDataStoreConfigToLegacy(dsConfig).GetCertConfig()

	if certConfig.GetCertPath() != "/tmp/mongo/tls.crt" {
		t.Fatalf("expected configured cert path, got %q", certConfig.GetCertPath())
	}

	if certConfig.GetKeyPath() != "/tmp/mongo/tls.key" {
		t.Fatalf("expected configured key path, got %q", certConfig.GetKeyPath())
	}

	if certConfig.GetCACertPath() != "/tmp/mongo/ca.crt" {
		t.Fatalf("expected configured CA path, got %q", certConfig.GetCACertPath())
	}
}
