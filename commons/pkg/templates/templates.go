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

package templates

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// LoadTemplate reads and parses a Go text/template file from mountPath/fileName. This function is used by
// fault-quarantine for post-remediation validation templates and by fault-remediation for remediation templates.
func LoadTemplate(mountPath, fileName, templateName string) (*template.Template, error) {
	templatePath := filepath.Join(mountPath, fileName)

	if _, err := os.Stat(templatePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("template file does not exist: %s", templatePath)
	}

	content, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("error reading template file: %w", err)
	}

	tmpl, err := template.New(templateName).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("error parsing template: %w", err)
	}

	return tmpl, nil
}

// Render executes the given template and decodes the resulting YAML document into an unstructured object.
func Render(tmpl *template.Template, data any) (*unstructured.Unstructured, string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, "", fmt.Errorf("error rendering template: %w", err)
	}

	var obj map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &obj); err != nil {
		return nil, "", fmt.Errorf("error unmarshalling rendered template: %w", err)
	}

	return &unstructured.Unstructured{Object: obj}, buf.String(), nil
}

func SetNodeOwnerRef(obj *unstructured.Unstructured, node *corev1.Node) {
	obj.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         "v1",
			Kind:               "Node",
			Name:               node.Name,
			UID:                node.UID,
			Controller:         new(false),
			BlockOwnerDeletion: new(false),
		},
	})
}
