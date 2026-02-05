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
Origin: https://github.com/kubernetes/code-generator/blob/v0.34.1/cmd/client-gen/generators/generator_for_group.go
*/

// Package generators has the generators for the client-gen utility.
package generators

import (
	"io"

	genutil "k8s.io/code-generator/pkg/util"
	"k8s.io/gengo/v2/generator"
	"k8s.io/gengo/v2/namer"
	"k8s.io/gengo/v2/types"

	"github.com/nvidia/nvsentinel/code-generator/cmd/client-gen/generators/util"
)

// genGroup produces a file for a group client, e.g. ExtensionsClient for the extension group.
type genGroup struct {
	generator.GoGenerator
	outputPackage string
	group         string
	version       string
	groupGoName   string
	apiPath       string
	// types in this group
	types            []*types.Type
	imports          namer.ImportTracker
	inputPackage     string
	clientsetPackage string // must be a Go import-path
	// If the genGroup has been called. This generator should only execute once.
	called bool
}

var _ generator.Generator = &genGroup{}

// We only want to call GenerateType() once per group.
func (g *genGroup) Filter(c *generator.Context, t *types.Type) bool {
	if !g.called {
		g.called = true
		return true
	}
	return false
}

func (g *genGroup) Namers(c *generator.Context) namer.NameSystems {
	return namer.NameSystems{
		"raw": namer.NewRawNamer(g.outputPackage, g.imports),
	}
}

func (g *genGroup) Imports(c *generator.Context) (imports []string) {
	imports = append(imports, g.imports.ImportLines()...)
	return
}

func (g *genGroup) GenerateType(c *generator.Context, t *types.Type, w io.Writer) error {
	sw := generator.NewSnippetWriter(w, c, "$", "$")

	// allow user to define a group name that's different from the one parsed from the directory.
	p := c.Universe.Package(g.inputPackage)
	groupName := g.group
	override, err := genutil.ExtractCommentTagsWithoutArguments("+", []string{"groupName"}, p.Comments)
	if err != nil {
		return err
	}
	if values, ok := override["groupName"]; ok {
		groupName = values[0]
	}

	m := map[string]interface{}{
		"version":             g.version,
		"groupName":           groupName,
		"GroupGoName":         g.groupGoName,
		"Version":             namer.IC(g.version),
		"types":               g.types,
		"ClientConnInterface": c.Universe.Type(types.Name{Package: "google.golang.org/grpc", Name: "ClientConnInterface"}),
		"Config":              c.Universe.Type(types.Name{Package: "github.com/nvidia/nvsentinel/client-go/nvgrpc", Name: "Config"}),
		"ClientConnFor":       c.Universe.Function(types.Name{Package: "github.com/nvidia/nvsentinel/client-go/nvgrpc", Name: "ClientConnFor"}),
		"Logger":              c.Universe.Type(types.Name{Package: "github.com/go-logr/logr", Name: "Logger"}),
		"Discard":             c.Universe.Function(types.Name{Package: "github.com/go-logr/logr", Name: "Discard"}),
		"fmtErrorf":           c.Universe.Function(types.Name{Package: "fmt", Name: "Errorf"}),
	}
	sw.Do(groupInterfaceTemplate, m)
	sw.Do(groupClientTemplate, m)
	for _, t := range g.types {
		tags, err := util.ParseClientGenTags(append(t.SecondClosestCommentLines, t.CommentLines...))
		if err != nil {
			return err
		}
		wrapper := map[string]interface{}{
			"type":        t,
			"GroupGoName": g.groupGoName,
			"Version":     namer.IC(g.version),
		}
		if tags.NonNamespaced {
			sw.Do(getterImplNonNamespaced, wrapper)
		} else {
			sw.Do(getterImplNamespaced, wrapper)
		}
	}
	sw.Do(newClientForConfigTemplate, m)
	sw.Do(newClientForConfigAndClientTemplate, m)
	sw.Do(newClientForConfigOrDieTemplate, m)
	sw.Do(newClientForGrpcConnTemplate, m)
	sw.Do(getClientConn, m)

	return sw.Error()
}

var groupInterfaceTemplate = `
type $.GroupGoName$$.Version$Interface interface {
	ClientConn() $.ClientConnInterface|raw$
	$range .types$ $.|publicPlural$Getter
	$end$
}
`

var groupClientTemplate = `
// $.GroupGoName$$.Version$Client is used to interact with features provided by the $.groupName$ group.
type $.GroupGoName$$.Version$Client struct {
	conn   $.ClientConnInterface|raw$
	logger $.Logger|raw$
}
`

var getterImplNamespaced = `
func (c *$.GroupGoName$$.Version$Client) $.type|publicPlural$(namespace string) $.type|public$Interface {
	return new$.type|publicPlural$(c, namespace)
}
`

var getterImplNonNamespaced = `
func (c *$.GroupGoName$$.Version$Client) $.type|publicPlural$() $.type|public$Interface {
	return new$.type|publicPlural$(c)
}
`

var newClientForConfigTemplate = `
// NewForConfig creates a new $.GroupGoName$$.Version$Client for the given config.
// NewForConfig is equivalent to NewForConfigAndClient(c, clientConn),
// where clientConn was generated with nvgrpc.ClientConnFor(c).
func NewForConfig(c *$.Config|raw$) (*$.GroupGoName$$.Version$Client, error) {
	if c == nil {
		return nil, $.fmtErrorf|raw$("config cannot be nil")
	}

	config := *c // Shallow copy to avoid mutation
	conn, err := $.ClientConnFor|raw$(&config)
	if err != nil {
		return nil, err
	}

	return NewForConfigAndClient(&config, conn)
}
`

var newClientForConfigAndClientTemplate = `
// NewForConfigAndClient creates a new $.GroupGoName$$.Version$Client for the given config and gRPC client connection.
// Note the grpc client connection provided takes precedence over the configured transport values.
func NewForConfigAndClient(c *$.Config|raw$, conn $.ClientConnInterface|raw$) (*$.GroupGoName$$.Version$Client, error) {
	if c == nil {
		return nil, $.fmtErrorf|raw$("config cannot be nil")
	}
	if conn == nil {
		return nil, $.fmtErrorf|raw$("gRPC connection cannot be nil")
	}

	return &$.GroupGoName$$.Version$Client{
		conn:   conn,
		logger: c.GetLogger().WithName("$.groupName$.$.version$"),
	}, nil
}
`

var newClientForConfigOrDieTemplate = `
// NewForConfigOrDie creates a new $.GroupGoName$$.Version$Client for the given config and
// panics if there is an error in the config.
func NewForConfigOrDie(c *$.Config|raw$) *$.GroupGoName$$.Version$Client {
	client, err := NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return client
}
`

var getClientConn = `
// ClientConn returns a gRPC client connection that is used to communicate
// with API server by this client implementation.
func (c *$.GroupGoName$$.Version$Client) ClientConn() $.ClientConnInterface|raw$ {
	if c == nil {
		return nil
	}
	return c.conn
}
`

var newClientForGrpcConnTemplate = `
// New creates a new $.GroupGoName$$.Version$Client for the given gRPC client connection.
func New(c $.ClientConnInterface|raw$) *$.GroupGoName$$.Version$Client {
	return &$.GroupGoName$$.Version$Client{
		conn:   c,
		logger: $.Discard|raw$(),
	}
}
`
