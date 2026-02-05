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
Origin: https://github.com/kubernetes/code-generator/blob/v0.34.1/cmd/client-gen/generators/generator_for_clientset.go
*/

// Package generators has the generators for the client-gen utility.
package generators

import (
	"fmt"
	"io"
	"path"
	"strings"

	clientgentypes "github.com/nvidia/nvsentinel/code-generator/cmd/client-gen/types"

	"k8s.io/gengo/v2/generator"
	"k8s.io/gengo/v2/namer"
	"k8s.io/gengo/v2/types"
)

// genClientset generates a package for a clientset.
type genClientset struct {
	generator.GoGenerator
	groups             []clientgentypes.GroupVersions
	groupGoNames       map[clientgentypes.GroupVersion]string
	clientsetPackage   string // must be a Go import-path
	imports            namer.ImportTracker
	clientsetGenerated bool
}

var _ generator.Generator = &genClientset{}

func (g *genClientset) Namers(c *generator.Context) namer.NameSystems {
	return namer.NameSystems{
		"raw": namer.NewRawNamer(g.clientsetPackage, g.imports),
	}
}

// We only want to call GenerateType() once.
func (g *genClientset) Filter(c *generator.Context, t *types.Type) bool {
	ret := !g.clientsetGenerated
	g.clientsetGenerated = true
	return ret
}

func (g *genClientset) Imports(c *generator.Context) (imports []string) {
	imports = append(imports, g.imports.ImportLines()...)
	for _, group := range g.groups {
		for _, version := range group.Versions {
			typedClientPath := path.Join(g.clientsetPackage, "typed", strings.ToLower(group.PackageName), strings.ToLower(version.NonEmpty()))
			groupAlias := strings.ToLower(g.groupGoNames[clientgentypes.GroupVersion{Group: group.Group, Version: version.Version}])
			imports = append(imports, fmt.Sprintf("%s%s \"%s\"", groupAlias, strings.ToLower(version.NonEmpty()), typedClientPath))
		}
	}
	return
}

func (g *genClientset) GenerateType(c *generator.Context, t *types.Type, w io.Writer) error {
	// TODO: We actually don't need any type information to generate the clientset,
	// perhaps we can adapt the go2ild framework to this kind of usage.
	sw := generator.NewSnippetWriter(w, c, "$", "$")

	allGroups := clientgentypes.ToGroupVersionInfo(g.groups, g.groupGoNames)
	m := map[string]interface{}{
		"allGroups":           allGroups,
		"fmtErrorf":           c.Universe.Type(types.Name{Package: "fmt", Name: "Errorf"}),
		"Config":              c.Universe.Type(types.Name{Package: "github.com/nvidia/nvsentinel/client-go/nvgrpc", Name: "Config"}),
		"ClientConnFor":       c.Universe.Function(types.Name{Package: "github.com/nvidia/nvsentinel/client-go/nvgrpc", Name: "ClientConnFor"}),
		"ClientConnInterface": c.Universe.Type(types.Name{Package: "google.golang.org/grpc", Name: "ClientConnInterface"}),
	}

	sw.Do(clientsetInterface, m)
	sw.Do(clientsetTemplate, m)
	for _, g := range allGroups {
		sw.Do(clientsetInterfaceImplTemplate, g)
	}
	sw.Do(newClientsetForConfigTemplate, m)
	sw.Do(newClientsetForConfigAndClientTemplate, m)
	sw.Do(newClientsetForConfigOrDieTemplate, m)
	sw.Do(newClientsetForGrpcClientTemplate, m)

	return sw.Error()
}

var clientsetInterface = `
type Interface interface {
	$range .allGroups$$.GroupGoName$$.Version$() $.PackageAlias$.$.GroupGoName$$.Version$Interface
	$end$
}
`
var clientsetTemplate = `
// Clientset contains the clients for groups.
type Clientset struct {
	$range .allGroups$$.LowerCaseGroupGoName$$.Version$ *$.PackageAlias$.$.GroupGoName$$.Version$Client
	$end$
}
`

var clientsetInterfaceImplTemplate = `
// $.GroupGoName$$.Version$ retrieves the $.GroupGoName$$.Version$Client
func (c *Clientset) $.GroupGoName$$.Version$() $.PackageAlias$.$.GroupGoName$$.Version$Interface {
	return c.$.LowerCaseGroupGoName$$.Version$
}
`

var newClientsetForConfigTemplate = `
// NewForConfig creates a new Clientset for the given config.
// NewForConfig is equivalent to NewForConfigAndClient(c, clientConn),
// where clientConn was generated with nvgrpc.ClientConnFor(c).
//
// If you need to customize the connection (e.g. set a logger),
// use nvgrpc.ClientConnFor() manually and pass the connection to NewForConfigAndClient.
func NewForConfig(c *$.Config|raw$) (*Clientset, error) {
	if c == nil {
		return nil, $.fmtErrorf|raw$("config cannot be nil")
	}

	configShallowCopy := *c // Shallow copy to avoid mutation
	conn, err := $.ClientConnFor|raw$(&configShallowCopy)
	if err != nil {
		return nil, err
	}

	return NewForConfigAndClient(&configShallowCopy, conn)
}
`

var newClientsetForConfigAndClientTemplate = `
// NewForConfigAndClient creates a new Clientset for the given config and gRPC client connection.
// The provided gRPC client connection provided takes precedence over the configured transport values.
func NewForConfigAndClient(c *$.Config|raw$, conn $.ClientConnInterface|raw$) (*Clientset, error) {
	if c == nil {
		return nil, $.fmtErrorf|raw$("config cannot be nil")
	}
	if conn == nil {
		return nil, $.fmtErrorf|raw$("gRPC connection cannot be nil")
	}

	configShallowCopy := *c // Shallow copy to avoid mutation
	
	var cs Clientset
	var err error
$range .allGroups$    cs.$.LowerCaseGroupGoName$$.Version$, err = $.PackageAlias$.NewForConfigAndClient(&configShallowCopy, conn)
	if err != nil {
		return nil, err
	}
$end$
	return &cs, nil
}
`

var newClientsetForConfigOrDieTemplate = `
// NewForConfigOrDie creates a new Clientset for the given config and
// panics if there is an error in the config or connection setup.
func NewForConfigOrDie(c *$.Config|raw$) *Clientset {
	cs, err := NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return cs
}
`

var newClientsetForGrpcClientTemplate = `
// New creates a new Clientset for the given gRPC client connection.
func New(conn $.ClientConnInterface|raw$) *Clientset {
	var cs Clientset
$range .allGroups$    cs.$.LowerCaseGroupGoName$$.Version$ = $.PackageAlias$.New(conn)
$end$
	return &cs
}
`
