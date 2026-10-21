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

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	draTestNamespace = "dra-test"
	migDeviceClass   = "mig.nvidia.com"
	nicDeviceClass   = "rdma.example.com"
)

// draTestEnv is an envtest API server that holds the ResourceClaims and
// ResourceClaimTemplates the DRA tests resolve.
type draTestEnv struct {
	env    *envtest.Environment
	client client.Client
}

// startDRATestEnv starts envtest and creates one namespace with the claims and
// templates used by the tests. The namespace allows adminAccess requests, so
// the adminAccess template is stored as written.
func startDRATestEnv(t *testing.T) *draTestEnv {
	t.Helper()

	env := &envtest.Environment{}
	restConfig, err := env.Start()
	require.NoError(t, err, "failed to start envtest")
	t.Cleanup(func() { require.NoError(t, env.Stop()) })

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	require.NoError(t, err)

	ctx := context.Background()

	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   draTestNamespace,
		Labels: map[string]string{"resource.kubernetes.io/admin-access": "true"},
	}}))

	templates := map[string][]resourcev1.DeviceRequest{
		"gpu-tmpl":         {exactRequest(model.GPUDRADriverName, false)},
		"nic-tmpl":         {exactRequest(nicDeviceClass, false)},
		"admin-gpu-tmpl":   {exactRequest(model.GPUDRADriverName, true)},
		"gpu-first-tmpl":   {firstAvailableRequest(model.GPUDRADriverName, model.GPUDRADriverName)},
		"mixed-first-tmpl": {firstAvailableRequest(model.GPUDRADriverName, nicDeviceClass)},
		"nic-and-gpu-tmpl": {exactRequest(nicDeviceClass, false), exactRequest(model.GPUDRADriverName, false)},
	}
	for name, requests := range templates {
		require.NoError(t, c.Create(ctx, &resourcev1.ResourceClaimTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: draTestNamespace},
			Spec: resourcev1.ResourceClaimTemplateSpec{
				Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: requests}},
			},
		}), "create template %s", name)
	}

	require.NoError(t, c.Create(ctx, &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-claim", Namespace: draTestNamespace},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{exactRequest(model.GPUDRADriverName, false)}},
		},
	}))

	// Confirm the API server kept adminAccess; without it the adminAccess case
	// would test an ordinary GPU request.
	var admin resourcev1.ResourceClaimTemplate
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: draTestNamespace, Name: "admin-gpu-tmpl"}, &admin))
	require.NotNil(t, admin.Spec.Spec.Devices.Requests[0].Exactly.AdminAccess)
	require.True(t, *admin.Spec.Spec.Devices.Requests[0].Exactly.AdminAccess)

	return &draTestEnv{env: env, client: c}
}

func exactRequest(deviceClass string, adminAccess bool) resourcev1.DeviceRequest {
	req := resourcev1.DeviceRequest{
		Name:    "req-" + deviceClassSuffix(deviceClass),
		Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: deviceClass},
	}
	if adminAccess {
		req.Exactly.AdminAccess = new(true)
	}

	return req
}

// firstAvailableRequest returns a request with one subrequest per class.
// Subrequest names carry the position, so a class can appear more than once.
func firstAvailableRequest(deviceClasses ...string) resourcev1.DeviceRequest {
	req := resourcev1.DeviceRequest{Name: "first"}
	for idx, deviceClass := range deviceClasses {
		req.FirstAvailable = append(req.FirstAvailable, resourcev1.DeviceSubRequest{
			Name:            deviceClassSuffix(deviceClass) + strconv.Itoa(idx),
			DeviceClassName: deviceClass,
		})
	}

	return req
}

// deviceClassSuffix turns a class name into a valid request name.
func deviceClassSuffix(deviceClass string) string {
	switch deviceClass {
	case model.GPUDRADriverName:
		return "gpu"
	case migDeviceClass:
		return "mig"
	default:
		return "nic"
	}
}

// draPod returns a pod with no extended GPU resources whose claims come from
// the given templates, in order. Each claim is named after its template.
func draPod(templates ...string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: draTestNamespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "train", Image: "training:latest"}},
		},
	}

	for _, tmpl := range templates {
		pod.Spec.ResourceClaims = append(pod.Spec.ResourceClaims, corev1.PodResourceClaim{
			Name:                      tmpl,
			ResourceClaimTemplateName: new(tmpl),
		})
	}

	for _, podClaim := range pod.Spec.ResourceClaims {
		pod.Spec.Containers[0].Resources.Claims = append(pod.Spec.Containers[0].Resources.Claims,
			corev1.ResourceClaim{Name: podClaim.Name})
	}

	return pod
}

// injectedInitContainers returns the init containers from the patch that
// creates spec.initContainers, or nil when there is no such patch.
func injectedInitContainers(t *testing.T, patches []PatchOperation) []corev1.Container {
	t.Helper()

	p := findPatchByPath(patches, "/spec/initContainers")
	if p == nil {
		return nil
	}

	containers, ok := p.Value.([]corev1.Container)
	require.True(t, ok)

	return containers
}

func claimNames(c corev1.Container) []string {
	names := make([]string, 0, len(c.Resources.Claims))
	for _, claim := range c.Resources.Claims {
		names = append(names, claim.Name)
	}

	return names
}

// countingReader counts reads before passing them to the API server.
type countingReader struct {
	client.Reader
	gets atomic.Int32
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.gets.Add(1)

	return r.Reader.Get(ctx, key, obj, opts...)
}

// deadlineReader records the deadline it was called with and fails as a slow
// API server would. envtest cannot make the API server slow on demand.
type deadlineReader struct {
	client.Reader
	deadline time.Time
}

func (r *deadlineReader) Get(ctx context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	r.deadline, _ = ctx.Deadline()

	return context.DeadlineExceeded
}

func TestInjectInitContainers_DRA(t *testing.T) {
	te := startDRATestEnv(t)

	tests := []struct {
		name           string
		pod            *corev1.Pod
		wantInjected   bool
		wantClaimNames []string
	}{
		// Must not inject: checks without GPU claims would report a false
		// fatal "no GPUs" event on a healthy node.
		{name: "no claims", pod: draPod()},
		{name: "non-GPU device class", pod: draPod("nic-tmpl")},
		{name: "adminAccess GPU request", pod: draPod("admin-gpu-tmpl")},
		{name: "firstAvailable with a non-GPU class", pod: draPod("mixed-first-tmpl")},
		{name: "template not found", pod: draPod("missing-tmpl")},
		{
			name: "claim not found",
			pod: func() *corev1.Pod {
				p := draPod()
				p.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: new("missing-claim")}}
				return p
			}(),
		},

		// Must inject, with every pod claim mirrored.
		{name: "GPU template", pod: draPod("gpu-tmpl"), wantInjected: true, wantClaimNames: []string{"gpu-tmpl"}},
		{
			name: "GPU claim",
			pod: func() *corev1.Pod {
				p := draPod()
				p.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpu", ResourceClaimName: new("gpu-claim")}}
				return p
			}(),
			wantInjected:   true,
			wantClaimNames: []string{"gpu"},
		},
		{
			name:           "firstAvailable with only GPU classes",
			pod:            draPod("gpu-first-tmpl"),
			wantInjected:   true,
			wantClaimNames: []string{"gpu-first-tmpl"},
		},
		{
			name:           "GPU request after a NIC request in one template",
			pod:            draPod("nic-and-gpu-tmpl"),
			wantInjected:   true,
			wantClaimNames: []string{"nic-and-gpu-tmpl"},
		},
		{
			name:           "NIC claim is mirrored with the GPU claim",
			pod:            draPod("nic-tmpl", "gpu-tmpl"),
			wantInjected:   true,
			wantClaimNames: []string{"nic-tmpl", "gpu-tmpl"},
		},
		{
			name:           "one confirmed GPU claim is enough when another lookup fails",
			pod:            draPod("missing-tmpl", "gpu-tmpl"),
			wantInjected:   true,
			wantClaimNames: []string{"missing-tmpl", "gpu-tmpl"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patches, _, err := NewInjector(testConfig(), nil, te.client).InjectInitContainers(context.Background(), tt.pod)
			require.NoError(t, err, "DRA lookups must never reject a pod")

			if !tt.wantInjected {
				assert.Empty(t, patches)
				return
			}

			containers := injectedInitContainers(t, patches)
			require.Len(t, containers, 1)
			assert.Equal(t, tt.wantClaimNames, claimNames(containers[0]))
			assert.NotContains(t, containers[0].Resources.Limits, corev1.ResourceName("nvidia.com/gpu"))
		})
	}

	t.Run("network extended resources are copied to the checks", func(t *testing.T) {
		pod := draPod("gpu-tmpl")
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"vpc.amazonaws.com/efa": resource.MustParse("4")}

		patches, _, err := NewInjector(testConfig(), nil, te.client).InjectInitContainers(context.Background(), pod)
		require.NoError(t, err)

		containers := injectedInitContainers(t, patches)
		require.Len(t, containers, 1)
		assert.Equal(t, resource.MustParse("4"), containers[0].Resources.Limits["vpc.amazonaws.com/efa"])
		assert.Equal(t, resource.MustParse("100m"), containers[0].Resources.Requests[corev1.ResourceCPU])
	})

	t.Run("reader without RBAC access skips injection", func(t *testing.T) {
		user, err := te.env.AddUser(envtest.User{Name: "no-dra-access"}, nil)
		require.NoError(t, err)

		scheme := runtime.NewScheme()
		utilruntime.Must(resourcev1.AddToScheme(scheme))

		forbidden, err := client.New(user.Config(), client.Options{Scheme: scheme})
		require.NoError(t, err)

		patches, _, err := NewInjector(testConfig(), nil, forbidden).InjectInitContainers(context.Background(), draPod("gpu-tmpl"))
		require.NoError(t, err)
		assert.Empty(t, patches)
	})

	t.Run("gang member gets each claim once", func(t *testing.T) {
		cfg := testGangConfig()
		resolver := gang.NewResolver(&mockDiscoverer{name: "test", canHandle: true, gangID: "gang-1"}, nil)

		patches, gangCtx, err := NewInjector(cfg, resolver, te.client).
			InjectInitContainers(context.Background(), draPod("nic-tmpl", "gpu-tmpl"))
		require.NoError(t, err)
		require.NotNil(t, gangCtx)

		containers := injectedInitContainers(t, patches)
		require.Len(t, containers, 1)
		assert.Equal(t, []string{"nic-tmpl", "gpu-tmpl"}, claimNames(containers[0]))
	})

	t.Run("gang member gets claims when gang mirroring is off", func(t *testing.T) {
		cfg := testGangConfig()
		cfg.GangCoordination.MirrorResourceClaims = new(false)
		resolver := gang.NewResolver(&mockDiscoverer{name: "test", canHandle: true, gangID: "gang-1"}, nil)

		patches, _, err := NewInjector(cfg, resolver, te.client).
			InjectInitContainers(context.Background(), draPod("gpu-tmpl"))
		require.NoError(t, err)

		containers := injectedInitContainers(t, patches)
		require.Len(t, containers, 1)
		assert.Equal(t, []string{"gpu-tmpl"}, claimNames(containers[0]))
	})

	t.Run("device plugin pod is unchanged and makes no API call", func(t *testing.T) {
		reader := &countingReader{Reader: te.client}
		pod := draPod("gpu-tmpl")
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}

		patches, _, err := NewInjector(testConfig(), nil, reader).InjectInitContainers(context.Background(), pod)
		require.NoError(t, err)

		containers := injectedInitContainers(t, patches)
		require.Len(t, containers, 1)
		assert.Empty(t, containers[0].Resources.Claims, "device plugin pods without gang mirroring get no claims")
		assert.Equal(t, resource.MustParse("8"), containers[0].Resources.Limits["nvidia.com/gpu"])
		assert.Zero(t, reader.gets.Load())
	})

	t.Run("pod without claims is unchanged and makes no API call", func(t *testing.T) {
		reader := &countingReader{Reader: te.client}

		patches, _, err := NewInjector(testConfig(), nil, reader).InjectInitContainers(context.Background(), draPod())
		require.NoError(t, err)
		assert.Empty(t, patches)
		assert.Zero(t, reader.gets.Load())
	})

	t.Run("nil reader skips injection", func(t *testing.T) {
		patches, _, err := NewInjector(testConfig(), nil, nil).InjectInitContainers(context.Background(), draPod("gpu-tmpl"))
		require.NoError(t, err)
		assert.Empty(t, patches)
	})
}

func TestInjectInitContainers_DRALookupTimesOut_SkipsInjection(t *testing.T) {
	reader := &deadlineReader{}
	start := time.Now()

	patches, _, err := NewInjector(testConfig(), nil, reader).InjectInitContainers(context.Background(), draPod("gpu-tmpl"))
	require.NoError(t, err)
	assert.Empty(t, patches)

	require.False(t, reader.deadline.IsZero(), "lookups must run under a deadline")
	assert.WithinDuration(t, start.Add(draLookupTimeout), reader.deadline, time.Second)
}

func TestIsGPURequest(t *testing.T) {
	tests := []struct {
		name string
		req  resourcev1.DeviceRequest
		want bool
	}{
		{name: "exact GPU class", req: exactRequest(model.GPUDRADriverName, false), want: true},
		{name: "exact MIG class is not detected", req: exactRequest(migDeviceClass, false), want: false},
		{name: "exact non-GPU class", req: exactRequest(nicDeviceClass, false), want: false},
		{name: "adminAccess GPU request", req: exactRequest(model.GPUDRADriverName, true), want: false},
		{
			name: "adminAccess explicitly false",
			req: resourcev1.DeviceRequest{Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: model.GPUDRADriverName,
				AdminAccess:     new(false),
			}},
			want: true,
		},
		{name: "firstAvailable all GPU", req: firstAvailableRequest(model.GPUDRADriverName, model.GPUDRADriverName), want: true},
		{name: "firstAvailable GPU or MIG", req: firstAvailableRequest(model.GPUDRADriverName, migDeviceClass), want: false},
		{name: "firstAvailable mixed", req: firstAvailableRequest(model.GPUDRADriverName, nicDeviceClass), want: false},
		{name: "empty request", req: resourcev1.DeviceRequest{}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isGPURequest(tt.req))
		})
	}
}

func TestMirrorResourceClaims_ExistingClaim_NotDuplicated(t *testing.T) {
	container := &corev1.Container{Resources: corev1.ResourceRequirements{
		Claims: []corev1.ResourceClaim{{Name: "gpu", Request: "req-gpu"}},
	}}

	mirrorResourceClaims(container, []corev1.PodResourceClaim{{Name: "gpu"}, {Name: "nic"}})

	assert.Equal(t, []corev1.ResourceClaim{{Name: "gpu", Request: "req-gpu"}, {Name: "nic"}}, container.Resources.Claims)
}

func TestHandleMutate_DRALookupFails_AdmitsWithoutPatch(t *testing.T) {
	te := startDRATestEnv(t)

	tests := []struct {
		name      string
		pod       *corev1.Pod
		wantPatch bool
	}{
		{name: "missing template admits the pod unchanged", pod: draPod("missing-tmpl")},
		{name: "GPU template admits the pod with checks", pod: draPod("gpu-tmpl"), wantPatch: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := handlerConfig()
			handler := NewHandler(cfg, nil, te.client, nil, nil)

			body := buildAdmissionReview(tt.pod, "uid-dra", draTestNamespace)
			req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
			rec := httptest.NewRecorder()

			handler.HandleMutate(rec, req)

			var review admissionv1.AdmissionReview
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &review))
			require.NotNil(t, review.Response)
			assert.True(t, review.Response.Allowed)

			if tt.wantPatch {
				assert.NotEmpty(t, review.Response.Patch)
			} else {
				assert.Empty(t, review.Response.Patch)
			}
		})
	}
}
