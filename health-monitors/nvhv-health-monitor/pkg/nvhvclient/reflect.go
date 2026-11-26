// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nvhvclient

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"google.golang.org/grpc"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// fetchFiles reads the schema of the service, with the files it imports, from
// the server by gRPC reflection.
func fetchFiles(ctx context.Context, conn grpc.ClientConnInterface, symbol string) (*protoregistry.Files, error) {
	stream, err := reflectionv1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("NVHV gRPC reflection: %w", err)
	}

	defer func() { _ = stream.CloseSend() }()

	err = stream.Send(&reflectionv1.ServerReflectionRequest{
		MessageRequest: &reflectionv1.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol},
	})
	if err != nil {
		return nil, fmt.Errorf("NVHV gRPC reflection: %w", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("NVHV gRPC reflection: %w", err)
	}

	if e := resp.GetErrorResponse(); e != nil {
		return nil, fmt.Errorf("NVHV gRPC reflection has no %s: %s", symbol, e.GetErrorMessage())
	}

	byPath := map[string]*descriptorpb.FileDescriptorProto{}

	for _, raw := range resp.GetFileDescriptorResponse().GetFileDescriptorProto() {
		fdp := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(raw, fdp); err != nil {
			return nil, fmt.Errorf("decode NVHV file descriptor: %w", err)
		}

		byPath[fdp.GetName()] = fdp
	}

	return buildFiles(byPath)
}

// buildFiles turns file descriptors into a registry, imports first. An import
// the server did not send must be one this binary knows, such as a well-known
// protobuf file.
func buildFiles(byPath map[string]*descriptorpb.FileDescriptorProto) (*protoregistry.Files, error) {
	files := &protoregistry.Files{}

	var build func(path string) error

	build = func(path string) error {
		if _, err := files.FindFileByPath(path); err == nil {
			return nil
		}

		fdp, ok := byPath[path]
		if !ok {
			known, err := protoregistry.GlobalFiles.FindFileByPath(path)
			if err != nil {
				return fmt.Errorf("NVHV did not send %s: %w", path, err)
			}

			return files.RegisterFile(known)
		}

		for _, dep := range fdp.GetDependency() {
			if err := build(dep); err != nil {
				return err
			}
		}

		fd, err := protodesc.NewFile(fdp, files)
		if err != nil {
			return fmt.Errorf("NVHV file %s: %w", path, err)
		}

		return files.RegisterFile(fd)
	}

	for _, path := range slices.Sorted(maps.Keys(byPath)) {
		if err := build(path); err != nil {
			return nil, err
		}
	}

	return files, nil
}
