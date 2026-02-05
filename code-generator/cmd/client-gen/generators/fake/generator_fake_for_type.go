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
Origin: https://github.com/kubernetes/code-generator/blob/v0.34.1/cmd/client-gen/generators/fake/generator_fake_for_type.go
*/

package fake

import (
	"io"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"k8s.io/gengo/v2/generator"
	"k8s.io/gengo/v2/namer"
	"k8s.io/gengo/v2/types"

	"github.com/nvidia/nvsentinel/code-generator/cmd/client-gen/generators/util"
)

// genFakeForType produces a file for each top-level type.
type genFakeForType struct {
	generator.GoGenerator
	outputPackage     string // Must be a Go import-path
	realClientPackage string // Must be a Go import-path
	version           string
	groupGoName       string
	inputPackage      string
	typeToMatch       *types.Type
	imports           namer.ImportTracker
}

var _ generator.Generator = &genFakeForType{}

var titler = cases.Title(language.Und)

// Filter ignores all but one type because we're making a single file per type.
func (g *genFakeForType) Filter(c *generator.Context, t *types.Type) bool { return t == g.typeToMatch }

func (g *genFakeForType) Namers(c *generator.Context) namer.NameSystems {
	return namer.NameSystems{
		"raw": namer.NewRawNamer(g.outputPackage, g.imports),
	}
}

func (g *genFakeForType) Imports(c *generator.Context) (imports []string) {
	return g.imports.ImportLines()
}

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

// GenerateType makes the body of a file implementing the individual typed client for type t.
func (g *genFakeForType) GenerateType(c *generator.Context, t *types.Type, w io.Writer) error {
	sw := generator.NewSnippetWriter(w, c, "$", "$")
	tags, err := util.ParseClientGenTags(append(t.SecondClosestCommentLines, t.CommentLines...))
	if err != nil {
		return err
	}

	const pkgClientGoTesting = "k8s.io/client-go/testing"
	m := map[string]interface{}{
		"type":                                      t,
		"inputType":                                 t,
		"resultType":                                t,
		"subresourcePath":                           "",
		"namespaced":                                !tags.NonNamespaced,
		"GroupGoName":                               g.groupGoName,
		"Version":                                   namer.IC(g.version),
		"realClientInterface":                       c.Universe.Type(types.Name{Package: g.realClientPackage, Name: t.Name.Name + "Interface"}),
		"SchemeGroupVersion":                        c.Universe.Type(types.Name{Package: t.Name.Package, Name: "SchemeGroupVersion"}),
		"contextContext":                            c.Universe.Type(types.Name{Package: "context", Name: "Context"}),
		"fmtErrorf":                                 c.Universe.Type(types.Name{Package: "fmt", Name: "Errorf"}),
		"GroupVersionResource":                      c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/runtime/schema", Name: "GroupVersionResource"}),
		"GroupVersionKind":                          c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/runtime/schema", Name: "GroupVersionKind"}),
		"watchInterface":                            c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/watch", Name: "Interface"}),
		"GetOptions":                                c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "GetOptions"}),
		"ListOptions":                               c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "ListOptions"}),
		"CreateOptions":                             c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "CreateOptions"}),
		"DeleteOptions":                             c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "DeleteOptions"}),
		"UpdateOptions":                             c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "UpdateOptions"}),
		"PatchOptions":                              c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Name: "PatchOptions"}),
		"PatchType":                                 c.Universe.Type(types.Name{Package: "k8s.io/apimachinery/pkg/types", Name: "PatchType"}),
		"NewRootListActionWithOptions":              c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewRootListActionWithOptions"}),
		"NewListActionWithOptions":                  c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewListActionWithOptions"}),
		"NewRootGetActionWithOptions":               c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewRootGetActionWithOptions"}),
		"NewGetActionWithOptions":                   c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewGetActionWithOptions"}),
		"NewRootDeleteActionWithOptions":            c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewRootDeleteActionWithOptions"}),
		"NewDeleteActionWithOptions":                c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewDeleteActionWithOptions"}),
		"NewRootUpdateActionWithOptions":            c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewRootUpdateActionWithOptions"}),
		"NewUpdateActionWithOptions":                c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewUpdateActionWithOptions"}),
		"NewRootCreateActionWithOptions":            c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewRootCreateActionWithOptions"}),
		"NewCreateActionWithOptions":                c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewCreateActionWithOptions"}),
		"NewRootWatchActionWithOptions":             c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewRootWatchActionWithOptions"}),
		"NewWatchActionWithOptions":                 c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewWatchActionWithOptions"}),
		"NewUpdateSubresourceActionWithOptions":     c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewUpdateSubresourceActionWithOptions"}),
		"NewRootUpdateSubresourceActionWithOptions": c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewRootUpdateSubresourceActionWithOptions"}),
		"NewPatchSubresourceActionWithOptions":      c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewPatchSubresourceActionWithOptions"}),
		"NewRootPatchSubresourceActionWithOptions":  c.Universe.Function(types.Name{Package: pkgClientGoTesting, Name: "NewRootPatchSubresourceActionWithOptions"}),
	}

	sw.Do(getterComment, m)
	if tags.NonNamespaced {
		sw.Do(getterNonNamespaced, m)
	} else {
		sw.Do(getterNamespaced, m)
	}

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

var getterComment = `
// $.type|publicPlural$Getter has a method to return a $.type|public$Interface.
// A group's client should implement this interface.`

var getterNamespaced = `
type $.type|publicPlural$Getter interface {
	$.type|publicPlural$(namespace string) $.realClientInterface|raw$
}
`

var getterNonNamespaced = `
type $.type|publicPlural$Getter interface {
	$.type|publicPlural$() $.realClientInterface|raw$
}
`

var structTemplateNamespaced = `
// fake$.type|publicPlural$ implements $.type|public$Interface
type fake$.type|publicPlural$ struct {
	Fake      *Fake$.GroupGoName$$.Version$
	namespace string
}
`

var structTemplateNonNamespaced = `
// fake$.type|publicPlural$ implements $.type|public$Interface
type fake$.type|publicPlural$ struct {
	Fake *Fake$.GroupGoName$$.Version$
}
`

var constructorTemplateNamespaced = `
// newFake$.type|publicPlural$ returns a fake$.type|publicPlural$
func newFake$.type|publicPlural$(fake *Fake$.GroupGoName$$.Version$, namespace string) $.realClientInterface|raw$ {
	return &fake$.type|publicPlural${
		Fake:      fake,
		namespace: namespace,
	}
}
`

var constructorTemplateNonNamespaced = `
// newFake$.type|publicPlural$ returns a fake$.type|publicPlural$
func newFake$.type|publicPlural$(fake *Fake$.GroupGoName$$.Version$) $.realClientInterface|raw$ {
	return &fake$.type|publicPlural${
		Fake: fake,
	}
}
`

var structHelpers = `
func (c *fake$.type|publicPlural$) Resource() $.GroupVersionResource|raw$ {
	return $.SchemeGroupVersion|raw$.WithResource("$.type|resource$")
}

func (c *fake$.type|publicPlural$) Kind() $.GroupVersionKind|raw$ {
	return $.SchemeGroupVersion|raw$.WithKind("$.type|singularKind$")
}

func (c *fake$.type|publicPlural$) GetNamespace() string {
	if c == nil {
		return ""
	}
	return $if .namespaced$c.namespace$else$""$end$
}
`

var listTemplate = `
// List takes label and field selectors, and returns the list of $.type|publicPlural$ that match those selectors.
func (c *fake$.type|publicPlural$) List(ctx $.contextContext|raw$, opts $.ListOptions|raw$) (result *$.type|raw$List, err error) {
	emptyResult := &$.type|raw$List{}
	obj, err := c.Fake.
		$if .namespaced$Invokes($.NewListActionWithOptions|raw$(c.Resource(), c.Kind(), c.GetNamespace(), opts), emptyResult)
		$else$Invokes($.NewRootListActionWithOptions|raw$(c.Resource(), c.Kind(), opts), emptyResult)$end$
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*$.type|raw$List), err
}
`

var getTemplate = `
// Get takes name of the $.type|public$, and returns the corresponding $.resultType|public$ object, and an error if there is any.
func (c *fake$.type|publicPlural$) Get(ctx $.contextContext|raw$, name string, opts $.GetOptions|raw$) (result *$.resultType|raw$, err error) {
	emptyResult := &$.resultType|raw${}
	obj, err := c.Fake.
		$if .namespaced$Invokes($.NewGetActionWithOptions|raw$(c.Resource(), c.GetNamespace(), name, opts), emptyResult)
		$else$Invokes($.NewRootGetActionWithOptions|raw$(c.Resource(), name, opts), emptyResult)$end$
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*$.resultType|raw$), err
}
`

var deleteTemplate = `
// Delete takes name of the $.type|private$ and deletes it. Returns an error if one occurs.
func (c *fake$.type|publicPlural$) Delete(ctx $.contextContext|raw$, name string, opts $.DeleteOptions|raw$) error {
	_, err := c.Fake.
		$if .namespaced$Invokes($.NewDeleteActionWithOptions|raw$(c.Resource(), c.GetNamespace(), name, opts), &$.type|raw${})
		$else$Invokes($.NewRootDeleteActionWithOptions|raw$(c.Resource(), name, opts), &$.type|raw${})$end$
	return err
}
`

var createTemplate = `
// Create takes the representation of a $.inputType|private$ and creates it.  Returns the server's representation of the $.resultType|private$, and an error, if there is any.
func (c *fake$.type|publicPlural$) Create(ctx $.contextContext|raw$, $.inputType|private$ *$.inputType|raw$, opts $.CreateOptions|raw$) (result *$.resultType|raw$, err error) {
	emptyResult := &$.resultType|raw${}
	obj, err := c.Fake.
		$if .namespaced$Invokes($.NewCreateActionWithOptions|raw$(c.Resource(), c.GetNamespace(), $.inputType|private$, opts), emptyResult)
		$else$Invokes($.NewRootCreateActionWithOptions|raw$(c.Resource(), $.inputType|private$, opts), emptyResult)$end$
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*$.resultType|raw$), err
}
`

var updateTemplate = `
// Update takes the representation of a $.inputType|private$ and updates it. Returns the server's representation of the $.resultType|private$, and an error, if there is any.
func (c *fake$.type|publicPlural$) Update(ctx $.contextContext|raw$, $.inputType|private$ *$.inputType|raw$, opts $.UpdateOptions|raw$) (result *$.resultType|raw$, err error) {
	emptyResult := &$.resultType|raw${}
	obj, err := c.Fake.
		$if .namespaced$Invokes($.NewUpdateActionWithOptions|raw$(c.Resource(), c.GetNamespace(), $.inputType|private$, opts), emptyResult)
		$else$Invokes($.NewRootUpdateActionWithOptions|raw$(c.Resource(), $.inputType|private$, opts), emptyResult)$end$
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*$.resultType|raw$), err
}
`

var updateStatusTemplate = `
func (c *fake$.type|publicPlural$) UpdateStatus(ctx $.contextContext|raw$, $.type|private$ *$.type|raw$, opts $.UpdateOptions|raw$) (*$.type|raw$, error) {
	obj, err := c.Fake.
		$if .namespaced$Invokes($.NewUpdateSubresourceActionWithOptions|raw$(c.Resource(), "status", c.GetNamespace(), $.type|private$, opts), &$.type|raw${})
		$else$Invokes($.NewRootUpdateSubresourceActionWithOptions|raw$(c.Resource(), "status", $.type|private$, opts), &$.type|raw${})$end$
	if obj == nil {
		return nil, err
	}
	return obj.(*$.type|raw$), err
}
`

var watchTemplate = `
// Watch returns a $.watchInterface|raw$ that watches the requested $.type|publicPlural$.
func (c *fake$.type|publicPlural$) Watch(ctx $.contextContext|raw$, opts $.ListOptions|raw$) ($.watchInterface|raw$, error) {
	return c.Fake.
		$if .namespaced$InvokesWatch($.NewWatchActionWithOptions|raw$($.SchemeGroupVersion|raw$.WithResource("$.type|resource$"), c.GetNamespace(), opts))
		$else$InvokesWatch($.NewRootWatchActionWithOptions|raw$($.SchemeGroupVersion|raw$.WithResource("$.type|resource$"), opts))$end$
}
`

var patchTemplate = `
// Patch applies the patch and returns the patched $.resultType|private$.
func (c *fake$.type|publicPlural$) Patch(ctx $.contextContext|raw$, name string, pt $.PatchType|raw$, data []byte, opts $.PatchOptions|raw$, subresources ...string) (result *$.resultType|raw$, err error) {
	emptyResult := &$.resultType|raw${}
	obj, err := c.Fake.
		$if .namespaced$Invokes($.NewPatchSubresourceActionWithOptions|raw$($.SchemeGroupVersion|raw$.WithResource("$.type|resource$"), c.GetNamespace(), name, pt, data, opts, subresources... ), emptyResult)
		$else$Invokes($.NewRootPatchSubresourceActionWithOptions|raw$($.SchemeGroupVersion|raw$.WithResource("$.type|resource$"), name, pt, data, opts, subresources...), emptyResult)$end$
	if obj == nil {
		return emptyResult, err
	}
	return obj.(*$.resultType|raw$), err
}
`
