// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package central

import (
	"context"
	"regexp"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// clientKeyPattern is the accepted shape of the client-supplied batch
// idempotency key. The key lands inside a server-derived composite, so its
// alphabet and length are a contract; "#" is the composite's separator.
var clientKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// clientIdempotencyKey reads and validates the batch's idempotency-key
// header. The header is mandatory: monitors retry over the network, and
// without the key a resent batch would be stored twice.
func clientIdempotencyKey(md metadata.MD) (string, error) {
	keys := md.Get(healthpub.IdempotencyKeyHeader)

	if len(keys) > 1 {
		return "", status.Error(codes.InvalidArgument, "multiple idempotency-key headers")
	}

	if len(keys) == 0 || keys[0] == "" {
		return "", status.Error(codes.InvalidArgument, "idempotency-key header is required")
	}

	if !clientKeyPattern.MatchString(keys[0]) {
		return "", status.Error(codes.InvalidArgument, "idempotency-key must match ^[A-Za-z0-9._:-]{1,128}$")
	}

	return keys[0], nil
}

// stampIdempotencyKeys writes the server-derived per-event key
// podUID#clientKey#eventIndex into each event's metadata, always overwriting
// any inbound value: the stored key is scoped to the authenticated caller, so
// an incoming value is never trusted.
func stampIdempotencyKeys(he *pb.HealthEvents, podUID, clientKey string) {
	batchKey := podUID + "#" + clientKey

	for i, ev := range he.GetEvents() {
		if ev.Metadata == nil {
			ev.Metadata = map[string]string{}
		}

		ev.Metadata[datastore.HealthEventIdempotencyKeyMetadataField] = store.EventIdempotencyKey(batchKey, i)
	}
}

// idempotencyInterceptor requires the batch idempotency-key header and stamps
// the server-derived per-event key into every event before the handler runs
// the pipeline: dedup reads the key to recognise a resend, and the store's
// unique index refuses a second copy. The pipeline never overwrites the key
// (the metadata transformer skips a node label of that name), so one stamp
// is enough. It runs after the auth interceptor, which put the caller's
// identity in the context.
func idempotencyInterceptor(
	ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (any, error) {
	he, ok := req.(*pb.HealthEvents)
	if !ok {
		return handler(ctx, req)
	}

	caller := auth.CallerFromContext(ctx)
	if caller == nil {
		return nil, status.Error(codes.Internal, "caller identity missing from context")
	}

	md, _ := metadata.FromIncomingContext(ctx)

	clientKey, err := clientIdempotencyKey(md)
	if err != nil {
		refusals.WithLabelValues(refusalIdempotencyKey).Inc()

		return nil, err
	}

	stampIdempotencyKeys(he, caller.PodUID, clientKey)

	return handler(ctx, req)
}

// readinessInterceptor refuses batches with a retryable status while the
// replica has not verified the idempotency index, so a resend is never stored
// twice before the index exists. Taking the replica out of the Service is not
// enough, because established connections keep sending to it.
func readinessInterceptor(gate *indexGate) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !gate.verified.Load() {
			refusals.WithLabelValues(refusalIndexUnverified).Inc()

			return nil, status.Error(codes.Unavailable, "idempotency index not verified on this replica; retry")
		}

		return handler(ctx, req)
	}
}
