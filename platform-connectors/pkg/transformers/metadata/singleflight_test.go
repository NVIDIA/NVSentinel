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

package metadata

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestGetOrFetchMetadata_SlowNodeDoesNotBlockOthers: a cache miss whose
// Kubernetes read hangs must not hold up a miss for a different node. The
// deployment platform connector runs this for the whole fleet, where one
// slow lookup used to stall every other node's events behind one lock. The
// hang is injected in the HTTP transport in front of the envtest API server,
// because the fake clientset serializes every call behind one lock itself.
func TestGetOrFetchMetadata_SlowNodeDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	slowNode, fastNode := "singleflight-slow", "singleflight-fast"

	for _, name := range []string{slowNode, fastNode} {
		_, err := testClient.CoreV1().Nodes().Create(ctx,
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		require.NoError(t, err)

		t.Cleanup(func() {
			_ = testClient.CoreV1().Nodes().Delete(context.Background(), name, metav1.DeleteOptions{})
		})
	}

	started := make(chan struct{})
	release := make(chan struct{})

	var startedOnce sync.Once

	restCfg := rest.CopyConfig(testEnv.Config)
	restCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/nodes/"+slowNode) {
				startedOnce.Do(func() { close(started) })
				<-release
			}

			return rt.RoundTrip(req)
		})
	})

	clientset, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	augmentor, err := New(ctx, &Config{CacheSize: 10, CacheTTL: time.Hour}, clientset)
	require.NoError(t, err)

	slowDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(ctx, slowNode)
		slowDone <- err
	}()

	<-started

	fastDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(ctx, fastNode)
		fastDone <- err
	}()

	select {
	case err := <-fastDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a lookup for another node waited behind the slow one")
	}

	close(release)
	require.NoError(t, <-slowDone)
}

// TestGetOrFetchMetadata_SameNodeSharesOneRead: concurrent misses for one
// node cost a single Kubernetes read, and every caller gets its result.
func TestGetOrFetchMetadata_SameNodeSharesOneRead(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	release := make(chan struct{})

	var gets atomic.Int32

	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-shared"},
	})
	clientset.PrependReactor("get", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		if gets.Add(1) == 1 {
			close(started)
		}

		<-release

		return false, nil, nil
	})

	augmentor, err := New(ctx, &Config{CacheSize: 10, CacheTTL: time.Hour}, clientset)
	require.NoError(t, err)

	const callers = 5

	var wg sync.WaitGroup

	results := make(chan *NodeMetadata, callers)

	for range callers {
		wg.Go(func() {
			metadata, err := augmentor.getOrFetchMetadata(ctx, "shared")
			if !assert.NoError(t, err) {
				return
			}
			results <- metadata
		})
	}

	<-started
	close(release)
	wg.Wait()
	close(results)

	for metadata := range results {
		require.Equal(t, "aws:///us-west-2a/i-shared", metadata.ProviderID)
	}

	require.EqualValues(t, 1, gets.Load(), "concurrent misses for one node share a single read")
}

// TestTransform_StalledLookupFailsOpenWithinTimeout: a node read that hangs
// ends with the lookup timeout, and the event proceeds without metadata
// (fail-open), so a stalled API server delays storage by at most that long.
func TestTransform_StalledLookupFailsOpenWithinTimeout(t *testing.T) {
	ctx := context.Background()
	node := "stalled-lookup"

	_, err := testClient.CoreV1().Nodes().Create(ctx,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = testClient.CoreV1().Nodes().Delete(context.Background(), node, metav1.DeleteOptions{})
	})

	restCfg := rest.CopyConfig(testEnv.Config)
	restCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/nodes/"+node) {
				// Hang until the request gives up.
				<-req.Context().Done()

				return nil, req.Context().Err()
			}

			return rt.RoundTrip(req)
		})
	})

	clientset, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	augmentor, err := New(ctx, &Config{CacheSize: 10, CacheTTL: time.Hour, LookupTimeout: 100 * time.Millisecond}, clientset)
	require.NoError(t, err)

	event := &pb.HealthEvent{NodeName: node, ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION}

	start := time.Now()
	require.NoError(t, augmentor.Transform(ctx, event), "a failed lookup never fails the event")
	require.Less(t, time.Since(start), 2*time.Second, "the lookup ends with its timeout")
	require.Empty(t, event.Metadata, "no metadata was added")
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, event.ProcessingStrategy, "the strategy is untouched")
}

// blockingNodeTransport wraps the envtest transport so reads of one node wait
// for release, honouring the request context.
func blockingNodeTransport(t *testing.T, node string, started chan<- struct{}, release <-chan struct{}) kubernetes.Interface {
	t.Helper()

	var startedOnce sync.Once

	restCfg := rest.CopyConfig(testEnv.Config)
	restCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/nodes/"+node) {
				startedOnce.Do(func() { close(started) })

				select {
				case <-release:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}

			return rt.RoundTrip(req)
		})
	})

	clientset, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	return clientset
}

func ensureNode(t *testing.T, name string) {
	t.Helper()

	_, err := testClient.CoreV1().Nodes().Create(context.Background(),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = testClient.CoreV1().Nodes().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

// TestGetOrFetchMetadata_LeaderCancellationDoesNotFailFollowers: the caller
// that started the shared read giving up must not fail the others waiting on
// the same node; the read continues, bounded by the lookup timeout, and they
// get their metadata.
func TestGetOrFetchMetadata_LeaderCancellationDoesNotFailFollowers(t *testing.T) {
	node := "shared-read-leader-cancel"
	ensureNode(t, node)

	started := make(chan struct{})
	release := make(chan struct{})
	clientset := blockingNodeTransport(t, node, started, release)

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour, LookupTimeout: 10 * time.Second}, clientset)
	require.NoError(t, err)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(leaderCtx, node)
		leaderDone <- err
	}()

	<-started

	followerDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(context.Background(), node)
		followerDone <- err
	}()

	cancelLeader()

	select {
	case err := <-leaderDone:
		require.ErrorIs(t, err, context.Canceled, "the leader leaves on its own context")
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled leader did not return")
	}

	select {
	case err := <-followerDone:
		t.Fatalf("the follower must keep waiting for the shared read, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-followerDone:
		require.NoError(t, err, "the follower gets the metadata from the shared read")
	case <-time.After(5 * time.Second):
		t.Fatal("the follower did not get its metadata")
	}

	_, found := augmentor.cache.Get(node)
	require.True(t, found, "the shared read still fills the cache")
}

// TestGetOrFetchMetadata_CancelledFollowerReturnsPromptly: a waiter whose own
// context ends leaves at once without stopping the shared read.
func TestGetOrFetchMetadata_CancelledFollowerReturnsPromptly(t *testing.T) {
	node := "shared-read-follower-cancel"
	ensureNode(t, node)

	started := make(chan struct{})
	release := make(chan struct{})
	clientset := blockingNodeTransport(t, node, started, release)

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour, LookupTimeout: 10 * time.Second}, clientset)
	require.NoError(t, err)

	leaderDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(context.Background(), node)
		leaderDone <- err
	}()

	<-started

	followerCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = augmentor.getOrFetchMetadata(followerCtx, node)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the follower leaves on its own deadline")

	close(release)
	require.NoError(t, <-leaderDone, "the shared read was not stopped by the follower leaving")
}

// TestTransform_SpentBudgetFailsOpenAtOnce: once the batch's pipeline budget
// is spent, a cache miss fails open immediately instead of waiting a whole
// lookup timeout per event.
func TestTransform_SpentBudgetFailsOpenAtOnce(t *testing.T) {
	node := "spent-budget"
	ensureNode(t, node)

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour}, testClient)
	require.NoError(t, err)

	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	event := &pb.HealthEvent{NodeName: node, ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION}

	start := time.Now()
	require.NoError(t, augmentor.Transform(spent, event))
	require.Less(t, time.Since(start), time.Second)
	require.Empty(t, event.Metadata)
}

// TestGetOrFetchMetadata_SpentBudgetStartsNoReads: once a batch's budget is
// spent, the remaining uncached nodes fail open without starting any read,
// detached or not.
func TestGetOrFetchMetadata_SpentBudgetStartsNoReads(t *testing.T) {
	var reads atomic.Int32

	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("get", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		reads.Add(1)

		return false, nil, nil
	})

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour}, clientset)
	require.NoError(t, err)

	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	for i := range 20 {
		event := &pb.HealthEvent{NodeName: fmt.Sprintf("uncached-%d", i)}
		require.NoError(t, augmentor.Transform(spent, event), "every event fails open")
	}

	time.Sleep(50 * time.Millisecond)
	require.Zero(t, reads.Load(), "no read is started for a caller that will not wait for it")
}

// TestPrewarm_ReadsEachUncachedNodeOnce: a batch naming several nodes, some
// repeatedly, costs one read per distinct node, and the per-event pass then
// finds everything cached.
func TestPrewarm_ReadsEachUncachedNodeOnce(t *testing.T) {
	var reads atomic.Int32

	clientset := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n3"}},
	)
	clientset.PrependReactor("get", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		reads.Add(1)

		return false, nil, nil
	})

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour}, clientset)
	require.NoError(t, err)

	events := []*pb.HealthEvent{
		{NodeName: "n1"}, {NodeName: "n2"}, {NodeName: "n1"}, {NodeName: "n3"}, {NodeName: "n2"}, {NodeName: ""},
	}

	require.NoError(t, augmentor.Prewarm(context.Background(), events))
	require.EqualValues(t, 3, reads.Load(), "one read per distinct node")

	for _, event := range events {
		if event.NodeName != "" {
			require.NoError(t, augmentor.Transform(context.Background(), event))
		}
	}

	require.EqualValues(t, 3, reads.Load(), "the per-event pass is served from the cache")
}

// TestPrewarm_ReadsNodesConcurrently: the distinct nodes of a batch are read
// at the same time, not one after another, so a cold batch costs about one
// lookup, not one per node.
func TestPrewarm_ReadsNodesConcurrently(t *testing.T) {
	nodes := []string{"prewarm-a", "prewarm-b", "prewarm-c"}
	for _, node := range nodes {
		ensureNode(t, node)
	}

	var inFlight atomic.Int32

	release := make(chan struct{})

	restCfg := rest.CopyConfig(testEnv.Config)
	restCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "/nodes/prewarm-") {
				inFlight.Add(1)
				defer inFlight.Add(-1)

				select {
				case <-release:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}

			return rt.RoundTrip(req)
		})
	})

	clientset, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour, LookupTimeout: 10 * time.Second}, clientset)
	require.NoError(t, err)

	events := make([]*pb.HealthEvent, 0, len(nodes))
	for _, node := range nodes {
		events = append(events, &pb.HealthEvent{NodeName: node})
	}

	done := make(chan struct{})

	go func() {
		_ = augmentor.Prewarm(context.Background(), events)
		close(done)
	}()

	require.Eventually(t, func() bool { return inFlight.Load() == int32(len(nodes)) },
		5*time.Second, time.Millisecond, "all nodes are read at once")

	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Prewarm did not return")
	}

	for _, node := range nodes {
		_, found := augmentor.cache.Get(node)
		require.True(t, found)
	}
}

// TestPrewarm_BoundsConcurrentReads: a batch naming more nodes than the bound
// reads them in waves; the number of reads in flight never exceeds the bound.
func TestPrewarm_BoundsConcurrentReads(t *testing.T) {
	const nodes = maxPrewarmReads + 8

	names := make([]string, 0, nodes)
	for i := range nodes {
		name := fmt.Sprintf("prewarm-bound-%d", i)
		ensureNode(t, name)
		names = append(names, name)
	}

	var inFlight, maxInFlight atomic.Int32

	release := make(chan struct{})

	restCfg := rest.CopyConfig(testEnv.Config)
	restCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "/nodes/prewarm-bound-") {
				now := inFlight.Add(1)
				defer inFlight.Add(-1)

				for {
					seen := maxInFlight.Load()
					if now <= seen || maxInFlight.CompareAndSwap(seen, now) {
						break
					}
				}

				select {
				case <-release:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}

			return rt.RoundTrip(req)
		})
	})

	clientset, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	augmentor, err := New(context.Background(), &Config{CacheSize: nodes, CacheTTL: time.Hour, LookupTimeout: 10 * time.Second}, clientset)
	require.NoError(t, err)

	events := make([]*pb.HealthEvent, 0, nodes)
	for _, name := range names {
		events = append(events, &pb.HealthEvent{NodeName: name})
	}

	done := make(chan struct{})

	go func() {
		_ = augmentor.Prewarm(context.Background(), events)
		close(done)
	}()

	require.Eventually(t, func() bool { return inFlight.Load() == maxPrewarmReads },
		5*time.Second, time.Millisecond, "the first wave fills the bound")
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, maxPrewarmReads, maxInFlight.Load(), "no read starts beyond the bound")

	close(release)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Prewarm did not return")
	}

	assert.EqualValues(t, maxPrewarmReads, maxInFlight.Load())

	for _, name := range names {
		_, found := augmentor.cache.Get(name)
		require.True(t, found, "%s was read", name)
	}
}

// TestPrewarm_SpentBudgetIsReported: the nodes a batch's budget ended before
// reading are reported, so the batch is deferred instead of those nodes
// passing the skip-label gate unread; the reads that had started finish
// detached and the resend finds them cached.
func TestPrewarm_SpentBudgetIsReported(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "slow-1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "slow-2"}},
	)
	clientset.PrependReactor("get", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		time.Sleep(150 * time.Millisecond)

		return false, nil, nil
	})

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour, LookupTimeout: 5 * time.Second}, clientset)
	require.NoError(t, err)

	events := []*pb.HealthEvent{{NodeName: "slow-1"}, {NodeName: "slow-2"}, {NodeName: "slow-1"}}

	budget, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err = augmentor.Prewarm(budget, events)
	require.ErrorIs(t, err, ErrBudgetExhausted)
	require.ErrorContains(t, err, "2 of 2 node(s)")

	// The detached reads finish on their own and warm the cache for the resend.
	require.Eventually(t, func() bool {
		_, found1 := augmentor.cache.Get("slow-1")
		_, found2 := augmentor.cache.Get("slow-2")

		return found1 && found2
	}, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, augmentor.Prewarm(context.Background(), events), "the resend finds every node cached")
}

// TestPrewarm_LookupFailureInsideTheBudgetIsNotReported: a read that fails
// while the budget is still running is a lookup failure, left to the
// per-event pass (which fails open on it), not a spent budget.
func TestPrewarm_LookupFailureInsideTheBudgetIsNotReported(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("get", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("the API server answered 500")
	})

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour}, clientset)
	require.NoError(t, err)

	budget, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, augmentor.Prewarm(budget, []*pb.HealthEvent{{NodeName: "broken"}}))
}
