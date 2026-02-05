/*
Copyright 2015 The Kubernetes Authors.

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

/*
Portions Copyright (c) 2025 NVIDIA CORPORATION. All rights reserved.

Modified from the original to support gRPC transport.
Origin: https://github.com/kubernetes/code-generator/blob/v0.34.1/cmd/client-gen/generators/fake/generator_fake_for_group.go
*/

package fake

import (
	"fmt"
	"io"
	"path"
	"strings"

	"k8s.io/gengo/v2/generator"
	"k8s.io/gengo/v2/namer"
	"k8s.io/gengo/v2/types"

	"github.com/nvidia/nvsentinel/code-generator/cmd/client-gen/generators/util"
)

// genFakeForGroup produces a file for a group client, e.g. ExtensionsClient for the extension group.
type genFakeForGroup struct {
	generator.GoGenerator
	outputPackage     string // must be a Go import-path
	realClientPackage string // must be a Go import-path
	version           string
	groupGoName       string
	// types in this group
	types   []*types.Type
	imports namer.ImportTracker
	// If the genGroup has been called. This generator should only execute once.
	called bool
}

var _ generator.Generator = &genFakeForGroup{}

// We only want to call GenerateType() once per group.
func (g *genFakeForGroup) Filter(c *generator.Context, t *types.Type) bool {
	if !g.called {
		g.called = true
		return true
	}
	return false
}

func (g *genFakeForGroup) Namers(c *generator.Context) namer.NameSystems {
	return namer.NameSystems{
		"raw": namer.NewRawNamer(g.outputPackage, g.imports),
	}
}

func (g *genFakeForGroup) Imports(c *generator.Context) (imports []string) {
	imports = g.imports.ImportLines()
	if len(g.types) != 0 {
		imports = append(imports, fmt.Sprintf("%s \"%s\"", strings.ToLower(path.Base(g.realClientPackage)), g.realClientPackage))
	}
	return imports
}

func (g *genFakeForGroup) GenerateType(c *generator.Context, t *types.Type, w io.Writer) error {
	sw := generator.NewSnippetWriter(w, c, "$", "$")

	m := map[string]interface{}{
		"GroupGoName": g.groupGoName,
		"Version":     namer.IC(g.version),
		"Fake":        c.Universe.Type(types.Name{Package: "k8s.io/client-go/testing", Name: "Fake"}),
		"grpc":        c.Universe.Type(types.Name{Package: "google.golang.org/grpc", Name: "ClientConnInterface"}),
	}

	sw.Do(groupClientTemplate, m)
	for _, t := range g.types {
		tags, err := util.ParseClientGenTags(append(t.SecondClosestCommentLines, t.CommentLines...))
		if err != nil {
			return err
		}
		wrapper := map[string]interface{}{
			"type":              t,
			"GroupGoName":       g.groupGoName,
			"Version":           namer.IC(g.version),
			"realClientPackage": strings.ToLower(path.Base(g.realClientPackage)),
		}
		if tags.NonNamespaced {
			sw.Do(getterImplNonNamespaced, wrapper)
			continue
		}
		sw.Do(getterImplNamespaced, wrapper)
	}
	return sw.Error()
}

var groupClientTemplate = `
type Fake$.GroupGoName$$.Version$ struct {
	*$.Fake|raw$
}

// ClientConn returns nil for fake clientsets. There is no real gRPC connection.
func (c *Fake$.GroupGoName$$.Version$) ClientConn() $.grpc|raw$ {
	return nil
}
`

var getterImplNamespaced = `
func (c *Fake$.GroupGoName$$.Version$) $.type|publicPlural$(namespace string) $.realClientPackage$.$.type|public$Interface {
	return newFake$.type|publicPlural$(c, namespace)
}
`

var getterImplNonNamespaced = `
func (c *Fake$.GroupGoName$$.Version$) $.type|publicPlural$() $.realClientPackage$.$.type|public$Interface {
	return newFake$.type|publicPlural$(c)
}
`
