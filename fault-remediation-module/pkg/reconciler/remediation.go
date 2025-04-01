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

package reconciler

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"text/template"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

type FaultRemediationClient struct {
	clientset    dynamic.Interface
	dryRunMode   []string
	template     *template.Template
	templateData TemplateData
}

// TemplateData holds the data to be inserted into the template
type TemplateData struct {
	NodeName          string
	Namespace         string
	Version           string
	ApiGroup          string
	TemplateMountPath string
	TemplateFileName  string
}

func NewK8sClient(kubeconfig string, dryRun bool, templateData TemplateData) (*FaultRemediationClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		if kubeconfig == "" {
			return nil, fmt.Errorf("kubeconfig is not set")
		}

		// build config from kubeconfig file
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("error creating Kubernetes config from kubeconfig: %w", err)
		}
	}

	clientset, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating clientset: %w", err)
	}

	// Construct full template path
	templatePath := filepath.Join(templateData.TemplateMountPath, templateData.TemplateFileName)

	// Read and parse the template
	templateContent, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("error reading template file: %w", err)
	}

	tmpl := template.New("maintenance").Delims("[[", "]]")
	tmpl, err = tmpl.Parse(string(templateContent))

	if err != nil {
		return nil, fmt.Errorf("error parsing template: %w", err)
	}

	client := &FaultRemediationClient{
		clientset:    clientset,
		template:     tmpl,
		templateData: templateData,
	}

	if dryRun {
		client.dryRunMode = []string{metav1.DryRunAll}
	} else {
		client.dryRunMode = []string{}
	}

	return client, nil
}

func (c *FaultRemediationClient) CreateMaintenanceResource(ctx context.Context, nodeName string) bool {
	log.Printf("Creating CR for node: %s", nodeName)

	c.templateData.NodeName = nodeName

	// Execute the template
	var buf bytes.Buffer
	if err := c.template.Execute(&buf, c.templateData); err != nil {
		log.Fatalf("Failed to execute template: %v", err)
		return false
	}

	// Convert YAML to unstructured
	var obj map[string]interface{}
	if err := yaml.Unmarshal(buf.Bytes(), &obj); err != nil {
		log.Fatalf("Failed to unmarshal YAML: %v", err)
		return false
	}

	maintenance := &unstructured.Unstructured{Object: obj}

	// Define the GVR for Maintenance resource
	gvr := schema.GroupVersionResource{
		Group:    c.templateData.ApiGroup,
		Version:  c.templateData.Version,
		Resource: "maintenances",
	}

	// Create the maintenance resource
	_, err := c.clientset.Resource(gvr).Namespace(c.templateData.Namespace).
		Create(ctx, maintenance, metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create Maintenance CR: %v", err)
		return false
	}

	log.Printf("Created Maintenance CR successfully for node %s", nodeName)
	return true
}
