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
	"io"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

func TestWatcher_NormalEvents(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan testEvent, 3)
	events <- testEvent{"ADDED", &FakeObject{Name: "obj1"}}
	events <- testEvent{"MODIFIED", &FakeObject{Name: "obj1"}}
	events <- testEvent{"DELETED", &FakeObject{Name: "obj1"}}
	close(events)

	source := &FakeSource{events: events, done: make(chan struct{})}
	w := NewWatcher(source, cancel, logr.Discard())

	var got []watch.Event
	for e := range w.ResultChan() {
		got = append(got, e)
	}

	wantTypes := []watch.EventType{watch.Added, watch.Modified, watch.Deleted}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d events, want %d", len(got), len(wantTypes))
	}
	for i, ev := range got {
		if ev.Type != wantTypes[i] {
			t.Errorf("event %d: got type %v, want %v", i, ev.Type, wantTypes[i])
		}
	}
}

func TestWatcher_UnknownEventType_Ignored(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan testEvent, 2)
	events <- testEvent{"UNKNOWN", &FakeObject{Name: "obj1"}}
	events <- testEvent{"ADDED", &FakeObject{Name: "obj2"}}
	close(events)

	source := &FakeSource{events: events, done: make(chan struct{})}
	w := NewWatcher(source, cancel, logr.Discard())

	var got []watch.Event
	for e := range w.ResultChan() {
		got = append(got, e)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Type != watch.Added {
		t.Errorf("expected ADDED, got %v", got[0].Type)
	}
}

func TestWatcher_Errors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason metav1.StatusReason
		wantCode   int32
	}{
		{"Internal", status.Error(codes.Internal, "err"), "", int32(codes.Internal)},
		{"OutOfRange", status.Error(codes.OutOfRange, "err"), metav1.StatusReasonExpired, 410},
		{"InvalidArgument", status.Error(codes.InvalidArgument, "err"), metav1.StatusReasonExpired, 410},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cancel := context.WithCancel(context.Background())
			defer cancel()

			source := &FakeSource{errs: []error{tc.err}, done: make(chan struct{})}
			w := NewWatcher(source, cancel, logr.Discard())

			e := <-w.ResultChan()
			if e.Type != watch.Error {
				t.Fatalf("expected watch.Error, got %v", e.Type)
			}
			st, ok := e.Object.(*metav1.Status)
			if !ok {
				t.Fatal("expected metav1.Status object")
			}
			if st.Reason != tc.wantReason || st.Code != tc.wantCode {
				t.Errorf("got %+v, wantReason %v, wantCode %d", st, tc.wantReason, tc.wantCode)
			}
		})
	}
}

func TestWatcher_ErrorTerminatesStream(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	source := &FakeSource{
		errs: []error{
			status.Error(codes.Internal, "fatal error"),
			status.Error(codes.Internal, "should never be reached"),
		},
		done: make(chan struct{}),
	}
	w := NewWatcher(source, cancel, logr.Discard())

	count := 0
	timeout := time.After(500 * time.Millisecond)

Receive:
	for {
		select {
		case _, ok := <-w.ResultChan():
			if !ok {
				break Receive
			}
			count++
		case <-timeout:
			t.Fatal("Test timed out waiting for ResultChan to close")
		}
	}

	if count != 1 {
		t.Errorf("expected 1 error event, got %d", count)
	}
}

func TestWatcher_Stop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := NewFakeSource()

	w := NewWatcher(source, cancel, logr.Discard())
	// Allow the receive loop to start
	time.Sleep(10 * time.Millisecond)
	w.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Error("context not cancelled after Stop()")
	}

	select {
	case _, ok := <-w.ResultChan():
		if ok {
			t.Error("ResultChan not closed")
		}
	case <-time.After(time.Second):
		t.Error("ResultChan hang")
	}
}

// FakeObject is a minimal implementation of runtime.Object.
type FakeObject struct {
	metav1.TypeMeta
	Name string
}

func (f *FakeObject) DeepCopyObject() runtime.Object {
	return &FakeObject{
		TypeMeta: f.TypeMeta,
		Name:     f.Name,
	}
}

type testEvent struct {
	eventType string
	obj       runtime.Object
}

// FakeSource implements nvgrpc.Source.
type FakeSource struct {
	events chan testEvent
	errs   []error
	done   chan struct{}
}

func NewFakeSource() *FakeSource {
	return &FakeSource{
		events: make(chan testEvent, 10),
		done:   make(chan struct{}),
	}
}

func (f *FakeSource) Next() (string, runtime.Object, error) {
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return "", nil, err
	}

	select {
	case <-f.done:
		return "", nil, io.EOF
	case e, ok := <-f.events:
		if !ok {
			return "", nil, io.EOF
		}
		return e.eventType, e.obj, nil
	}
}

func (f *FakeSource) Close() error {
	if f.done == nil {
		return nil
	}
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}
