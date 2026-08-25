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

module csp-api-mock

go 1.27.0

require (
	cloud.google.com/go/logging v1.19.1
	google.golang.org/genproto v0.0.0-20260819154853-08b0e4226688
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260819154853-08b0e4226688
	google.golang.org/grpc v1.83.1
	google.golang.org/protobuf v1.36.12
)

require (
	cloud.google.com/go/iam v1.13.0 // indirect
	cloud.google.com/go/longrunning v1.2.0 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)
