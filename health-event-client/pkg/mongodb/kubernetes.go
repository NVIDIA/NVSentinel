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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// kubernetesClient implements KubernetesClient interface
type kubernetesClient struct {
	client *kubernetes.Clientset
}

// NewKubernetesClient creates a new Kubernetes client
func NewKubernetesSecretClient() (KubernetesSecretClient, error) {
	config, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s config: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s clientset: %w", err)
	}

	return &kubernetesClient{
		client: client,
	}, nil
}

// GetSecret retrieves a secret from Kubernetes
func (k *kubernetesClient) GetSecret(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	secret, err := k.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("MongoDB error during get_secret: %w", err)
	}

	return secret.Data, nil
}
