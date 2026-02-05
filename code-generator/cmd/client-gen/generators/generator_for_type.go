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
Origin: https://github.com/kubernetes/code-generator/blob/v0.34.1/cmd/client-gen/generators/generator_for_type.go
*/

// Package generators has the generators for the client-gen utility.
package generators

import (
	"io"
	"path"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"k8s.io/gengo/v2/generator"
	"k8s.io/gengo/v2/namer"
	"k8s.io/gengo/v2/types"

	"github.com/nvidia/nvsentinel/code-generator/cmd/client-gen/generators/util"
	clientgentypes "github.com/nvidia/nvsentinel/code-generator/cmd/client-gen/types"
)

// genClientForType produces a file for each top-level type.
type genClientForType struct {
	generator.GoGenerator
	outputPackage string // must be a Go import-path
	inputPackage  string
	group         string
	version       string
	groupGoName   string
	typeToMatch   *types.Type
	imports       namer.ImportTracker
	protoPackage  clientgentypes.ProtobufPackage
}

var _ generator.Generator = &genClientForType{}

var titler = cases.Title(language.Und)

// Filter ignores all but one type because we're making a single file per type.
func (g *genClientForType) Filter(c *generator.Context, t *types.Type) bool {
	return t == g.typeToMatch
}

func (g *genClientForType) Namers(c *generator.Context) namer.NameSystems {
	return namer.NameSystems{
		"raw": namer.NewRawNamer(g.outputPackage, g.imports),
	}
}

func (g *genClientForType) Imports(c *generator.Context) (imports []string) {
	return append(
		g.imports.ImportLines(),
		g.protoPackage.ImportLines()...,
	)
}

// Ideally, we'd like genStatus to return true if there is a subresource path
// registered for "status" in the API server, but we do not have that
// information, so genStatus returns true if the type has a status field.
func genStatus(t *types.Type) bool {
	// Default to true if we have a Status member
	hasStatus := false
	for _, m := range t.Members {
		if m.Name == "Status" {
			hasStatus = true
			break
		}
	}
	return hasStatus && !util.MustParseClientGenTags(append(t.SecondClosestCommentLines, t.CommentLines...)).NoStatus
}

func (g *genClientForType) GenerateType(c *generator.Context, t *types.Type, w io.Writer) error {
	sw := generator.NewSnippetWriter(w, c, "$", "$")
	pkg := path.Base(t.Name.Package)
	tags, err := util.ParseClientGenTags(append(t.SecondClosestCommentLines, t.CommentLines...))
	if err != nil {
		return err
	}
	protoType := titler.String(t.Name.Name)
	m := map[string]interface{}{
		"type":             t,
		"inputType":        t,
		"resultType":       t,
		"package":          pkg,
		"Package":          namer.IC(pkg),
		"namespaced":       !tags.NonNamespaced,
		"Group":            namer.IC(g.group),
		"GroupGoName":      g.groupGoName,
		"Version":          namer.IC(g.version),
		"ProtoType":        protoType,
		"context":          c.Universe.Type(types.Name{Package: "context", Name: "Context"}),
		"fmtErrorf":        c.Universe.Function(types.Name{Package: "fmt", Name: "Errorf"}),
		"metav1":           c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "Option"}),
		"GetOptions":       c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "GetOptions"}),
		"ListOptions":      c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "ListOptions"}),
		"CreateOptions":    c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "CreateOptions"}),
		"DeleteOptions":    c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "DeleteOptions"}),
		"UpdateOptions":    c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "UpdateOptions"}),
		"PatchOptions":     c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "PatchOptions"}),
		"PatchType":        c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/types", Name: "PatchType"}),
		"runtime":          c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/runtime", Name: "Object"}),
		"watchInterface":   c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/watch", Name: "Interface"}),
		"logr":             c.Universe.Type(types.Name{Package: "github.com/go-logr/logr", Name: "Logger"}),
		"nvgrpc":           c.Universe.Type(types.Name{Package: "github.com/nvidia/nvsentinel/client-go/nvgrpc", Name: "NewWatcher"}),
		"pb":               g.protoPackage.Alias,
		"apiPackage":       c.Universe.Type(types.Name{Package: g.inputPackage, Name: "Ignored"}),
		"FromProto":        c.Universe.Function(types.Name{Package: g.inputPackage, Name: "FromProto"}),
		"FromProtoList":    c.Universe.Function(types.Name{Package: g.inputPackage, Name: "FromProtoList"}),
		"NewServiceClient": g.protoPackage.ServiceClientConstructorFor(protoType),
	}

	sw.Do(getterComment, m)
	if tags.NonNamespaced {
		sw.Do(getterNonNamespaced, m)
	} else {
		sw.Do(getterNamespaced, m)
	}

	sw.Do(generateInterfaceTemplate(tags, t), m)

	if tags.NonNamespaced {
		sw.Do(structTemplateNonNamespaced, m)
		sw.Do(constructorTemplateNonNamespaced, m)
	} else {
		sw.Do(structTemplateNamespaced, m)
		sw.Do(constructorTemplateNamespaced, m)
	}

	sw.Do(structHelpers, m)

	if tags.HasVerb("get") {
		sw.Do(getTemplate, m)
	}

	if tags.HasVerb("list") {
		sw.Do(listTemplate, m)
	}

	if tags.HasVerb("watch") {
		sw.Do(watchTemplate, m)
		sw.Do(watchAdapterTemplate, m)
	}

	if tags.HasVerb("create") {
		sw.Do(createTemplate, m)
	}

	if tags.HasVerb("update") {
		sw.Do(updateTemplate, m)
	}

	if genStatus(t) && tags.HasVerb("updateStatus") {
		sw.Do(updateStatusTemplate, m)
	}

	if tags.HasVerb("delete") {
		sw.Do(deleteTemplate, m)
	}

	if tags.HasVerb("patch") {
		sw.Do(patchTemplate, m)
	}

	return sw.Error()
}

func generateInterfaceTemplate(tags util.Tags, t *types.Type) string {
	lines := []string{}

	lines = append(lines, `
// $.type|public$Interface has methods to work with $.type|public$ resources.
type $.type|public$Interface interface {`)

	if tags.HasVerb("create") {
		lines = append(lines, `	Create(ctx $.context|raw$, $.type|private$ *$.type|raw$, opts $.CreateOptions|raw$) (*$.type|raw$, error)`)
	}
	if tags.HasVerb("update") {
		lines = append(lines, `	Update(ctx $.context|raw$, $.type|private$ *$.type|raw$, opts $.UpdateOptions|raw$) (*$.type|raw$, error)`)
	}
	if genStatus(t) && tags.HasVerb("updateStatus") {
		lines = append(lines, `	UpdateStatus(ctx $.context|raw$, $.type|private$ *$.type|raw$, opts $.UpdateOptions|raw$) (*$.type|raw$, error)`)
	}
	if tags.HasVerb("delete") {
		lines = append(lines, `	Delete(ctx $.context|raw$, name string, opts $.DeleteOptions|raw$) error`)
	}
	if tags.HasVerb("get") {
		lines = append(lines, `	Get(ctx $.context|raw$, name string, opts $.GetOptions|raw$) (*$.type|raw$, error)`)
	}
	if tags.HasVerb("list") {
		lines = append(lines, `	List(ctx $.context|raw$, opts $.ListOptions|raw$) (*$.type|raw$List, error)`)
	}
	if tags.HasVerb("watch") {
		lines = append(lines, `	Watch(ctx $.context|raw$, opts $.ListOptions|raw$) ($.watchInterface|raw$, error)`)
	}
	if tags.HasVerb("patch") {
		lines = append(lines, `	Patch(ctx $.context|raw$, name string, pt $.PatchType|raw$, data []byte, opts $.PatchOptions|raw$, subresources ...string) (result *$.type|raw$, err error)`)
	}

	lines = append(lines, `	$.type|public$Expansion
}
`)

	return strings.Join(lines, "\n")
}

var getterComment = `
// $.type|publicPlural$Getter has a method to return a $.type|public$Interface.
// A group's client should implement this interface.`

var getterNamespaced = `
type $.type|publicPlural$Getter interface {
	$.type|publicPlural$(namespace string) $.type|public$Interface
}
`

var getterNonNamespaced = `
type $.type|publicPlural$Getter interface {
	$.type|publicPlural$() $.type|public$Interface
}
`

var structTemplateNamespaced = `
// $.type|allLowercasePlural$ implements $.type|public$Interface
type $.type|allLowercasePlural$ struct {
	client    $.pb$.$.ProtoType$ServiceClient
	logger    $.logr|raw$
	namespace string
}
`

var structTemplateNonNamespaced = `
// $.type|allLowercasePlural$ implements $.type|public$Interface
type $.type|allLowercasePlural$ struct {
	client $.pb$.$.ProtoType$ServiceClient
	logger $.logr|raw$
}
`

var constructorTemplateNamespaced = `
// new$.type|publicPlural$ returns a $.type|allLowercasePlural$
func new$.type|publicPlural$(c *$.GroupGoName$$.Version$Client, namespace string) *$.type|allLowercasePlural$ {
	return &$.type|allLowercasePlural${
		client:    $.NewServiceClient$(c.ClientConn()),
		logger:    c.logger.WithName("$.type|allLowercasePlural$"),
		namespace: namespace,
	}
}
`

var constructorTemplateNonNamespaced = `
// new$.type|publicPlural$ returns a $.type|allLowercasePlural$
func new$.type|publicPlural$(c *$.GroupGoName$$.Version$Client) *$.type|allLowercasePlural$ {
	return &$.type|allLowercasePlural${
		client: $.NewServiceClient$(c.ClientConn()),
		logger: c.logger.WithName("$.type|allLowercasePlural$"),
	}
}
`

var structHelpers = `
func (c *$.type|allLowercasePlural$) getNamespace() string {
	if c == nil {
		return ""
	}
	return $if .namespaced$c.namespace$else$""$end$
}
`

var listTemplate = `
func (c *$.type|allLowercasePlural$) List(ctx $.context|raw$, opts $.ListOptions|raw$) (*$.type|raw$List, error) {
	resp, err := c.client.List$.ProtoType$s(ctx, &$.pb$.List$.ProtoType$sRequest{
		Opts: &$.pb$.ListOptions{
			ResourceVersion: opts.ResourceVersion,
			Namespace:       c.getNamespace(),
		},
	})
	if err != nil {
		return nil, err
	}

	list := $.FromProtoList|raw$(resp.Get$.ProtoType$List())
	c.logger.V(5).Info("Listed $.type|public$s",
		"namespace", c.getNamespace(),
		"count", len(list.Items),
		"resource-version", list.GetResourceVersion(),
	)

	return list, nil
}
`

var getTemplate = `
func (c *$.type|allLowercasePlural$) Get(ctx $.context|raw$, name string, opts $.GetOptions|raw$) (*$.type|raw$, error) {
	resp, err := c.client.Get$.ProtoType$(ctx, &$.pb$.Get$.ProtoType$Request{
		Name: name,
		Opts: &$.pb$.GetOptions{
			ResourceVersion: opts.ResourceVersion,
			Namespace:       c.getNamespace(),
		},
	})
	if err != nil {
		return nil, err
	}

	obj := $.FromProto|raw$(resp.Get$.ProtoType$())
	c.logger.V(6).Info("Fetched $.type|public$",
		"name", name,
		"namespace", c.getNamespace(),
		"resource-version", obj.GetResourceVersion(),
	)

	return obj, nil
}
`

var deleteTemplate = `
func (c *$.type|allLowercasePlural$) Delete(ctx $.context|raw$, name string, opts $.DeleteOptions|raw$) error {
	return $.fmtErrorf|raw$("Delete not implemented for gRPC transport")
}
`

var createTemplate = `
func (c *$.type|allLowercasePlural$) Create(ctx $.context|raw$, $.type|allLowercasePlural$ *$.type|raw$, opts $.CreateOptions|raw$) (*$.type|raw$, error) {
	return nil, $.fmtErrorf|raw$("Create not implemented for gRPC transport")
}
`

var updateTemplate = `
func (c *$.type|allLowercasePlural$) Update(ctx $.context|raw$, $.type|allLowercasePlural$ *$.type|raw$, opts $.UpdateOptions|raw$) (*$.type|raw$, error) {
	return nil, $.fmtErrorf|raw$("Update not implemented for gRPC transport")
}
`

var updateStatusTemplate = `
func (c *$.type|allLowercasePlural$) UpdateStatus(ctx $.context|raw$, $.type|allLowercasePlural$ *$.type|raw$, opts $.UpdateOptions|raw$) (*$.type|raw$, error) {
	return nil, $.fmtErrorf|raw$("UpdateStatus not implemented for gRPC transport")
}
`

var watchTemplate = `
func (c *$.type|allLowercasePlural$) Watch(ctx $.context|raw$, opts $.ListOptions|raw$) ($.watchInterface|raw$, error) {
	c.logger.V(4).Info("Opening watch stream",
		"resource", "$.type|allLowercasePlural$",
		"namespace", c.getNamespace(),
		"resource-version", opts.ResourceVersion,
	)

	ctx, cancel := context.WithCancel(ctx)
	stream, err := c.client.Watch$.ProtoType$s(ctx, &$.pb$.Watch$.ProtoType$sRequest{
		Opts: &$.pb$.ListOptions{
			ResourceVersion: opts.ResourceVersion,
			Namespace:       c.getNamespace(),
		},
	})
	if err != nil {
		cancel()
		return nil, err
	}

	return $.nvgrpc|raw$(&$.type|allLowercasePlural$StreamAdapter{stream: stream}, cancel, c.logger), nil
}
`

var watchAdapterTemplate = `
// $.type|allLowercasePlural$StreamAdapter wraps the $.type|public$ gRPC stream to provide events.
type $.type|allLowercasePlural$StreamAdapter struct {
	stream $.pb$.$.ProtoType$Service_Watch$.ProtoType$sClient
}

func (a *$.type|allLowercasePlural$StreamAdapter) Next() (string, $.runtime|raw$, error) {
	resp, err := a.stream.Recv()
	if err != nil {
		return "", nil, err
	}

	obj := $.FromProto|raw$(resp.GetObject())

	return resp.GetType(), obj, nil
}

func (a *$.type|allLowercasePlural$StreamAdapter) Close() error {
	return a.stream.CloseSend()
}
`

var patchTemplate = `
func (c *$.type|allLowercasePlural$) Patch(ctx $.context|raw$, name string, pt $.PatchType|raw$, data []byte, opts $.PatchOptions|raw$, subresources ...string) (result *$.type|raw$, err error) {
	return nil, $.fmtErrorf|raw$("Patch not implemented for gRPC transport")
}
`
