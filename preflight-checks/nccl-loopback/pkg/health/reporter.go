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

package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	agentName      = "preflight-nccl-loopback"
	componentClass = "Node"
	checkName      = "NCCLLoopbackTest"
)

// Socket mode timing. These are variables so tests can shorten them.
var (
	// socketWaitTimeout bounds the wait for a socket file that is missing at
	// send time.
	socketWaitTimeout  = 20 * time.Second
	socketPollInterval = 500 * time.Millisecond

	// socketSendTimeout bounds one socket mode Publish call; healthpub sets no
	// deadline on the socket path.
	socketSendTimeout = 3 * time.Minute
)

// Reporter sends the check's health event through the shared healthpub client.
// Direct mode (see healthpub.DialFromEnvOr) leaves timeouts and retries to
// healthpub; socket mode bounds each send and waits for a missing socket itself.
type Reporter struct {
	publisher          *healthpub.Publisher
	nodeName           string
	processingStrategy pb.ProcessingStrategy
	direct             bool
	socketPath         string
}

// NewReporter dials the platform connector and builds a Reporter. tokenPath is
// the optional file path of a projected ServiceAccount token to present as a
// Bearer credential on every socket mode call; empty disables token metadata.
func NewReporter(socketPath, nodeName string, strategy pb.ProcessingStrategy, tokenPath string) (*Reporter, error) {
	// Remove unix:// prefix if present so the target is built the same way
	// for both forms of the socket setting.
	socketPath = strings.TrimPrefix(socketPath, "unix://")
	target := "unix://" + socketPath
	direct := true

	_, client, opt, err := healthpub.DialFromEnvOr(func() (*grpc.ClientConn, error) {
		// DialFromEnvOr runs the fallback only in socket mode.
		direct = false

		return grpc.NewClient(target, grpcclient.InsecureDialOptions(tokenPath)...)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to platform connector: %w", err)
	}

	return &Reporter{
		publisher:          healthpub.New(client, target, agentName, opt),
		nodeName:           nodeName,
		processingStrategy: strategy,
		direct:             direct,
		socketPath:         socketPath,
	}, nil
}

// waitForSocket waits up to socketWaitTimeout for the socket file when a send
// finds it missing. A socket still missing at the end is not an error: the
// following Publish reports the connector unavailable, which keeps the old
// send failure exit code. Only a finished ctx is an error.
func waitForSocket(ctx context.Context, socketPath string) error {
	err := wait.PollUntilContextTimeout(ctx, socketPollInterval, socketWaitTimeout, true,
		func(context.Context) (bool, error) {
			_, statErr := os.Stat(socketPath)

			return statErr == nil, nil
		})
	if err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return fmt.Errorf("waiting for platform connector socket %s: %w", socketPath, ctx.Err())
	}

	slog.Warn("Platform connector socket not found after waiting; sending anyway",
		"socket", socketPath, "waited", socketWaitTimeout)

	return nil
}

// Close releases the connection to the platform connector.
func (r *Reporter) Close() {
	r.publisher.CloseOrWarn()
}

func (r *Reporter) SendEvent(ctx context.Context, isHealthy, isFatal bool, message string, errorCode string) error {
	recommendedAction := pb.RecommendedAction_NONE
	if !isHealthy {
		recommendedAction = pb.RecommendedAction_CONTACT_SUPPORT
	}

	var errorCodes []string
	if errorCode != "" {
		errorCodes = []string{errorCode}
	}

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              agentName,
		ComponentClass:     componentClass,
		CheckName:          checkName,
		IsFatal:            isFatal,
		IsHealthy:          isHealthy,
		Message:            message,
		RecommendedAction:  recommendedAction,
		ErrorCode:          errorCodes,
		GeneratedTimestamp: timestamppb.Now(),
		NodeName:           r.nodeName,
		ProcessingStrategy: r.processingStrategy,
		EntitiesImpacted:   []*pb.Entity{},
	}

	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}

	slog.Info("Sending health event",
		"is_healthy", isHealthy,
		"is_fatal", isFatal,
		"message", message,
		"error_code", errorCode,
		"recommended_action", pb.RecommendedAction_name[int32(recommendedAction)])

	// Every error, including healthpub.ErrPlatformConnectorUnavailable, is a
	// send failure: a one-shot check has no later poll to re-emit the event.
	if err := r.publish(ctx, healthEvents); err != nil {
		return fmt.Errorf("failed to send health event: %w", err)
	}

	slog.Info("Health event sent successfully")

	return nil
}

// publish hands the batch to healthpub; in socket mode it waits once for a
// socket that is missing at send time and retries the send.
func (r *Reporter) publish(ctx context.Context, events *pb.HealthEvents) error {
	if r.direct {
		return r.publisher.Publish(ctx, events)
	}

	err := r.publishOverSocket(ctx, events)
	if !errors.Is(err, healthpub.ErrPlatformConnectorUnavailable) {
		return err
	}

	slog.Warn("Platform connector socket missing at send time; waiting for it to come back",
		"socket", r.socketPath, "wait", socketWaitTimeout)

	if waitErr := waitForSocket(ctx, r.socketPath); waitErr != nil {
		return waitErr
	}

	return r.publishOverSocket(ctx, events)
}

// publishOverSocket runs one socket mode Publish under socketSendTimeout.
func (r *Reporter) publishOverSocket(ctx context.Context, events *pb.HealthEvents) error {
	sendCtx, cancel := context.WithTimeout(ctx, socketSendTimeout)
	defer cancel()

	return r.publisher.Publish(sendCtx, events)
}
