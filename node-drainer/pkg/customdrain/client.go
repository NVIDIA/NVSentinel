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

package customdrain

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

type Client struct {
	dynamicClient dynamic.Interface
	restMapper    *restmapper.DeferredDiscoveryRESTMapper
	template      *template.Template
	config        config.CustomDrainConfig
}

func NewClient(
	cfg config.CustomDrainConfig,
	dynamicClient dynamic.Interface,
	restMapper *restmapper.DeferredDiscoveryRESTMapper,
) (*Client, error) {
	templatePath := filepath.Join(cfg.TemplateMountPath, cfg.TemplateFileName)

	templateContent, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read template file %s: %w", templatePath, err)
	}

	tmpl, err := template.New("drain-template").Parse(string(templateContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse template: %w", err)
	}

	gvk := schema.GroupVersionKind{
		Group:   cfg.ApiGroup,
		Version: cfg.Version,
		Kind:    cfg.Kind,
	}

	_, err = restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		slog.Error("Failed to validate custom drain CRD",
			"apiGroup", cfg.ApiGroup,
			"version", cfg.Version,
			"kind", cfg.Kind,
			"error", err)

		return nil, fmt.Errorf("failed to find rest mapping for custom drain CRD: %w", err)
	} else {
		slog.Info("Successfully validated custom drain CRD exists",
			"apiGroup", cfg.ApiGroup,
			"version", cfg.Version,
			"kind", cfg.Kind)
	}

	return &Client{
		dynamicClient: dynamicClient,
		restMapper:    restMapper,
		template:      tmpl,
		config:        cfg,
	}, nil
}

func (c *Client) CreateDrainCR(ctx context.Context, data TemplateData) (string, error) {
	var buf bytes.Buffer
	if err := c.template.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	var obj map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &obj); err != nil {
		return "", fmt.Errorf("failed to unmarshal rendered template: %w", err)
	}

	cr := &unstructured.Unstructured{Object: obj}

	crName := GenerateCRName(data.HealthEvent.NodeName, data.EventID)
	cr.SetName(crName)

	if cr.GetNamespace() == "" {
		cr.SetNamespace(c.config.Namespace)
	}

	gvk := schema.GroupVersionKind{
		Group:   c.config.ApiGroup,
		Version: c.config.Version,
		Kind:    c.config.Kind,
	}
	cr.SetGroupVersionKind(gvk)

	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return "", fmt.Errorf("failed to get REST mapping for %s: %w", gvk, err)
	}

	_, err = c.dynamicClient.
		Resource(mapping.Resource).
		Namespace(c.config.Namespace).
		Create(ctx, cr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create CR: %w", err)
	}

	return crName, nil
}

func (c *Client) Exists(ctx context.Context, crName string) (bool, error) {
	gvk := schema.GroupVersionKind{
		Group:   c.config.ApiGroup,
		Version: c.config.Version,
		Kind:    c.config.Kind,
	}

	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false, fmt.Errorf("failed to get REST mapping for %s: %w", gvk, err)
	}

	_, err = c.dynamicClient.
		Resource(mapping.Resource).
		Namespace(c.config.Namespace).
		Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("failed to check CR existence: %w", err)
	}

	return true, nil
}

func (c *Client) matchesCondition(condMap map[string]any) bool {
	condType, typeFound, _ := unstructured.NestedString(condMap, "type")
	condStatus, statusFound, _ := unstructured.NestedString(condMap, "status")

	return typeFound && statusFound &&
		condType == c.config.StatusConditionType &&
		strings.EqualFold(condStatus, c.config.StatusConditionStatus)
}

func (c *Client) GetCRStatus(ctx context.Context, crName string) (bool, error) {
	gvk := schema.GroupVersionKind{
		Group:   c.config.ApiGroup,
		Version: c.config.Version,
		Kind:    c.config.Kind,
	}

	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false, fmt.Errorf("failed to get REST mapping for %s: %w", gvk, err)
	}

	cr, err := c.dynamicClient.
		Resource(mapping.Resource).
		Namespace(c.config.Namespace).
		Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get CR: %w", err)
	}

	conditions, found, err := unstructured.NestedSlice(cr.Object, "status", "conditions")
	if err != nil {
		return false, fmt.Errorf("failed to extract status.conditions: %w", err)
	}

	if !found {
		return false, nil
	}

	for _, cond := range conditions {
		condMap, ok := cond.(map[string]any)
		if ok && c.matchesCondition(condMap) {
			return true, nil
		}
	}

	return false, nil
}

func (c *Client) DeleteDrainCR(ctx context.Context, crName string) error {
	gvk := schema.GroupVersionKind{
		Group:   c.config.ApiGroup,
		Version: c.config.Version,
		Kind:    c.config.Kind,
	}

	mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("failed to get REST mapping for %s: %w", gvk, err)
	}

	err = c.dynamicClient.
		Resource(mapping.Resource).
		Namespace(c.config.Namespace).
		Delete(ctx, crName, metav1.DeleteOptions{})

	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete CR: %w", err)
	}

	return nil
}
