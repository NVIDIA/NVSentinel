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

	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

type FaultRemediationClient struct {
	clientset    dynamic.Interface
	restMapper   *restmapper.DeferredDiscoveryRESTMapper
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
	RecommendedAction platformconnector.RecommenedAction
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

	// Create discovery client for RESTMapper
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating discovery client: %w", err)
	}

	// Create RESTMapper for GVK to GVR conversion
	cachedClient := memory.NewMemCacheClient(discoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)

	// Construct full template path
	templatePath := filepath.Join(templateData.TemplateMountPath, templateData.TemplateFileName)

	// Check if the template file exists
	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("template file does not exist: %s", templatePath)
	}

	// Read and parse the template
	templateContent, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("error reading template file: %w", err)
	}

	tmpl := template.New("maintenance")
	tmpl, err = tmpl.Parse(string(templateContent))
	if err != nil {
		return nil, fmt.Errorf("error parsing template: %w", err)
	}

	client := &FaultRemediationClient{
		clientset:    clientset,
		restMapper:   mapper,
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

func (c *FaultRemediationClient) CreateMaintenanceResource(ctx context.Context, healthEvent *platformconnector.HealthEvent) bool {
	// Skip custom resource creation if dry-run is enabled
	if len(c.dryRunMode) > 0 {
		log.Printf("DRY-RUN: Skipping custom resource creation for node %s", healthEvent.NodeName)
		return true
	}

	log.Printf("Creating RebootNode CR for node: %s", healthEvent.NodeName)
	c.templateData.NodeName = healthEvent.NodeName
	c.templateData.RecommendedAction = healthEvent.RecommendedAction

	// Execute the template
	var buf bytes.Buffer
	if err := c.template.Execute(&buf, c.templateData); err != nil {
		log.Fatalf("Failed to execute template: %v", err)
		return false
	}

	log.Printf("Generated YAML: %s", buf.String())

	// Convert YAML to unstructured
	var obj map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &obj); err != nil {
		log.Fatalf("Failed to unmarshal YAML: %v", err)
		return false
	}

	maintenance := &unstructured.Unstructured{Object: obj}

	// Get GVK from the unstructured object
	gvk := maintenance.GroupVersionKind()

	// Convert GVK to GVR using RESTMapper
	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		log.Fatalf("Failed to get REST mapping for %s: %v", gvk, err)
		return false
	}

	// Create the maintenance resource at cluster level
	_, err = c.clientset.Resource(mapping.Resource).
		Create(ctx, maintenance, metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create Maintenance CR: %v", err)
		return false
	}

	log.Printf("Created Maintenance CR successfully for node %s", healthEvent.NodeName)
	return true
}
