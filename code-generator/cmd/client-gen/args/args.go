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
Origin: https://github.com/kubernetes/code-generator/blob/v0.34.1/cmd/client-gen/args/args.go
*/

package args

import (
	"fmt"

	"github.com/spf13/pflag"

	"github.com/nvidia/nvsentinel/code-generator/cmd/client-gen/types"
)

type Args struct {
	// The directory for the generated results.
	OutputDir string

	// The Go import-path of the generated results.
	OutputPkg string

	// The boilerplate header for Go files.
	GoHeaderFile string

	// A sorted list of group versions to generate. For each of them the package path is found
	// in GroupVersionToInputPath.
	Groups []types.GroupVersions

	// Overrides for which types should be included in the client.
	IncludedTypesOverrides map[types.GroupVersion][]string

	// ClientsetName is the name of the clientset to be generated. It's
	// populated from command-line arguments.
	ClientsetName string
	// ClientsetAPIPath is the default API HTTP path for generated clients.
	ClientsetAPIPath string
	// ClientsetOnly determines if we should generate the clients for groups and
	// types along with the clientset. It's populated from command-line
	// arguments.
	ClientsetOnly bool
	// FakeClient determines if client-gen generates the fake clients.
	FakeClient bool
	// PluralExceptions specify list of exceptions used when pluralizing certain types.
	// For example 'Endpoints:Endpoints', otherwise the pluralizer will generate 'Endpointes'.
	PluralExceptions []string

	// ProtoBase is the base Go import-path of the protobuf stubs.
	ProtoBase string
}

func New() *Args {
	return &Args{
		ClientsetName:    "internalclientset",
		ClientsetAPIPath: "/apis",
		ClientsetOnly:    false,
		FakeClient:       true,
	}
}

func (a *Args) AddFlags(fs *pflag.FlagSet, inputBase string) {
	gvsBuilder := NewGroupVersionsBuilder(&a.Groups)
	fs.StringVar(&a.OutputDir, "output-dir", "",
		"the base directory under which to generate results")
	fs.StringVar(&a.OutputPkg, "output-pkg", a.OutputPkg,
		"the Go import-path of the generated results")
	fs.StringVar(&a.GoHeaderFile, "go-header-file", "",
		"the path to a file containing boilerplate header text; the string \"YEAR\" will be replaced with the current 4-digit year")
	fs.Var(NewGVPackagesValue(gvsBuilder, nil), "input",
		`group/versions that client-gen will generate clients for. At most one version per group is allowed. Specified in the format "group1/version1,group2/version2...".`)
	fs.Var(NewGVTypesValue(&a.IncludedTypesOverrides, []string{}), "included-types-overrides",
		"list of group/version/type for which client should be generated. By default, client is generated for all types which have genclient in types.go. This overrides that. For each groupVersion in this list, only the types mentioned here will be included. The default check of genclient will be used for other group versions.")
	fs.Var(NewInputBasePathValue(gvsBuilder, inputBase), "input-base",
		"base path to look for the api group.")
	fs.StringVarP(&a.ClientsetName, "clientset-name", "n", a.ClientsetName,
		"the name of the generated clientset package.")
	fs.StringVarP(&a.ClientsetAPIPath, "clientset-api-path", "", a.ClientsetAPIPath,
		"the value of default API HTTP path, starting with / and without trailing /.")
	fs.BoolVar(&a.ClientsetOnly, "clientset-only", a.ClientsetOnly,
		"when set, client-gen only generates the clientset shell, without generating the individual typed clients")
	fs.BoolVar(&a.FakeClient, "fake-clientset", a.FakeClient,
		"when set, client-gen will generate the fake clientset that can be used in tests")
	fs.StringSliceVar(&a.PluralExceptions, "plural-exceptions", a.PluralExceptions,
		"list of comma separated plural exception definitions in Type:PluralizedType form")
	fs.StringVar(&a.ProtoBase, "proto-base", "",
		"the base Go import-path of the protobuf stubs")

	// support old flags
	fs.SetNormalizeFunc(mapFlagName("clientset-path", "output-pkg", fs.GetNormalizeFunc()))
}

func (a *Args) Validate() error {
	if len(a.OutputDir) == 0 {
		return fmt.Errorf("--output-dir must be specified")
	}
	if len(a.OutputPkg) == 0 {
		return fmt.Errorf("--output-pkg must be specified")
	}
	if len(a.ClientsetName) == 0 {
		return fmt.Errorf("--clientset-name must be specified")
	}
	if len(a.ClientsetAPIPath) == 0 {
		return fmt.Errorf("--clientset-api-path cannot be empty")
	}
	if len(a.ProtoBase) == 0 {
		return fmt.Errorf("--proto-base must be specified")
	}

	return nil
}

// GroupVersionPackages returns a map from GroupVersion to the package with the types.go.
func (a *Args) GroupVersionPackages() map[types.GroupVersion]string {
	res := map[types.GroupVersion]string{}
	for _, pkg := range a.Groups {
		for _, v := range pkg.Versions {
			res[types.GroupVersion{Group: pkg.Group, Version: v.Version}] = v.Package
		}
	}
	return res
}

func mapFlagName(from, to string, old func(fs *pflag.FlagSet, name string) pflag.NormalizedName) func(fs *pflag.FlagSet, name string) pflag.NormalizedName {
	return func(fs *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == from {
			name = to
		}
		return old(fs, name)
	}
}
