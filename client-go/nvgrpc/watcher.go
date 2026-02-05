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

package nvgrpc

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

// Source defines the interface for reading events from a source.
type Source interface {
	Next() (eventType string, obj runtime.Object, err error)
	Close() error
}

// Watcher implements watch.Interface.
type Watcher struct {
	cancel   context.CancelFunc // cancels the event source
	result   chan watch.Event   // channel delivering watch events
	source   Source             // the underlying event source
	done     chan struct{}      // closed when watcher stops
	stopOnce sync.Once          // ensures stop is idempotent
	logger   logr.Logger
}

// NewWatcher creates a Watcher and starts receiving events.
func NewWatcher(
	source Source,
	cancel context.CancelFunc,
	logger logr.Logger,
) watch.Interface {
	w := &Watcher{
		cancel: cancel,
		result: make(chan watch.Event, 100),
		source: source,
		done:   make(chan struct{}),
		logger: logger.WithName("watcher"),
	}

	go w.receive()

	return w
}

// Stop cancels the context and closes the event source.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		w.logger.V(4).Info("Stopping watcher")
		w.cancel()

		if err := w.source.Close(); err != nil {
			w.logger.V(4).Info("Error closing source during stop", "err", err)
		}

		close(w.done)
	})
}

// ResultChan returns the channel delivering watch events.
func (w *Watcher) ResultChan() <-chan watch.Event {
	return w.result
}

// receive reads events from the source and sends them to result channel.
//
// nolint:cyclop // Complexity is necessary to handle various gRPC stream states and event types.
func (w *Watcher) receive() {
	defer func() {
		w.logger.V(4).Info("Watcher receive loop exiting")
		close(w.result)
	}()
	defer w.Stop()

	for {
		w.logger.V(6).Info("Waiting for next event from source")

		typeStr, obj, err := w.source.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled {
				w.logger.V(3).Info("Watch stream closed normally")
				return
			}

			w.logger.Error(err, "Watch stream encountered unexpected error")
			w.sendError(err)

			return
		}

		var eventType watch.EventType

		switch typeStr {
		case "ADDED":
			eventType = watch.Added
		case "MODIFIED":
			eventType = watch.Modified
		case "DELETED":
			eventType = watch.Deleted
		case "ERROR":
			w.logger.V(4).Info("Received explicit ERROR event from server")

			w.result <- watch.Event{Type: watch.Error, Object: obj}

			return
		default:
			w.logger.V(2).Info("Skipping unknown event type from server", "rawType", typeStr)
			continue
		}

		select {
		case <-w.done:
			w.logger.V(3).Info("Watcher stopping; aborting receive loop")
			return
		case w.result <- watch.Event{Type: eventType, Object: obj}:
			if meta, ok := obj.(metav1.Object); ok {
				w.logger.V(6).Info("Event dispatched to Informer",
					"type", eventType,
					"name", meta.GetName(),
					"resourceVersion", meta.GetResourceVersion(),
				)
			}
		}
	}
}

func (w *Watcher) sendError(err error) {
	st := status.Convert(err)

	code := st.Code()
	statusErr := &metav1.Status{
		Status:  metav1.StatusFailure,
		Message: st.Message(),
		Code:    int32(code), // #nosec G115
	}

	//nolint:exhaustive // Only specific gRPC codes require special Kubernetes status mapping.
	switch code {
	case codes.OutOfRange, codes.ResourceExhausted, codes.InvalidArgument:
		// CRITICAL for Informers: This tells the Reflector to perform a new List operation.
		statusErr.Reason = metav1.StatusReasonExpired
		statusErr.Code = 410
	case codes.PermissionDenied:
		statusErr.Reason = metav1.StatusReasonForbidden
		statusErr.Code = 403
	case codes.NotFound:
		statusErr.Reason = metav1.StatusReasonNotFound
		statusErr.Code = 404
	default:
		w.logger.V(5).Info("Using default status mapping for gRPC code", "code", code)
	}

	w.logger.V(4).Info("Sending error event to informer",
		"code", statusErr.Code,
		"reason", statusErr.Reason,
		"message", statusErr.Message,
	)

	select {
	case <-w.done:
		w.logger.V(4).Info("Watcher already done, dropping error event")
	case w.result <- watch.Event{Type: watch.Error, Object: statusErr}:
	}
}
