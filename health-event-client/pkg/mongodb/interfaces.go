/*
Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mongodb

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// MongoDBRepository defines the interface for MongoDB operations
type MongoDBRepository interface {
	Connect(ctx context.Context) error
	Disconnect(ctx context.Context) error
	FindDocumentsByNodeName(ctx context.Context, nodeName string) ([]bson.M, error)
	QueryHealthEvents(ctx context.Context, filter bson.M, limit int) ([]bson.M, error)
	GetCollection() *mongo.Collection
	GetClient() *mongo.Client
	IsConnected() bool
}

// KubernetesClient defines interface for Kubernetes operations
type KubernetesSecretClient interface {
	GetSecret(ctx context.Context, namespace, name string) (map[string][]byte, error)
}

// CertificateManager defines interface for certificate file operations
type CertificateManager interface {
	WriteCertificateFiles(caCert, tlsCert, tlsKey []byte) error
	CleanupCertificateFiles() error
}
