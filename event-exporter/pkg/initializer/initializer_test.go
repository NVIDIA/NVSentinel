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

package initializer

import (
	"testing"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestTokenDatabaseCertMountPath_NoTLSReturnsEmptyPath(t *testing.T) {
	dsConfig := &datastore.DataStoreConfig{
		Provider: datastore.ProviderMongoDB,
	}

	if got := tokenDatabaseCertMountPath(dsConfig); got != "" {
		t.Fatalf("expected empty cert mount path when TLS is disabled, got %q", got)
	}
}

func TestTokenDatabaseCertMountPath_TLSConfigReturnsCADirectory(t *testing.T) {
	dsConfig := &datastore.DataStoreConfig{
		Provider: datastore.ProviderMongoDB,
		Connection: datastore.ConnectionConfig{
			TLSConfig: &datastore.TLSConfig{
				CAPath: "/tmp/mongo-certs/ca.crt",
			},
		},
	}

	if got := tokenDatabaseCertMountPath(dsConfig); got != "/tmp/mongo-certs" {
		t.Fatalf("expected CA cert directory, got %q", got)
	}
}
