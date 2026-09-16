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

package mapper

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/kubelet/pkg/apis/podresources/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

// grantTestPodPatch authorizes only the test identity's annotation writes in one namespace.
func grantTestPodPatch(t *testing.T, admin kubernetes.Interface, namespace, user string) {
	t.Helper()

	_, err := admin.RbacV1().Roles(namespace).Create(t.Context(), &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "patch-pods"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"patch"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = admin.RbacV1().RoleBindings(namespace).Create(t.Context(), &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "patch-pods"},
		Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: user}},
		RoleRef:    rbacv1.RoleRef{Kind: "Role", APIGroup: rbacv1.GroupName, Name: "patch-pods"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func TestHostMapper_SeparateCredentials_RequiresPodPatchPermission(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	environment := &envtest.Environment{}
	adminConfig, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, environment.Stop()) })

	admin, err := kubernetes.NewForConfig(adminConfig)
	require.NoError(t, err)
	_, err = admin.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "host-auth"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	pod, err := admin.CoreV1().Pods("host-auth").Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-workload"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "workload", Image: "test.invalid/workload"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	user, err := environment.AddUser(envtest.User{Name: "metadata-test"}, adminConfig)
	require.NoError(t, err)

	kubelet := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-kubelet-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		assert.NoError(t, json.NewEncoder(w).Encode(corev1.PodList{Items: []corev1.Pod{*pod}}))
	}))
	defer kubelet.Close()

	mapperInterface, err := NewPodDeviceMapper(t.Context(), WithKubeconfigs(
		writeTestKubeconfig(t, user.Config()), writeTestKubeconfig(t, tlsServerConfig(kubelet))))
	require.NoError(t, err)
	mapper := mapperInterface.(*podDeviceMapper)

	// Only the Unix socket location changes for the fixture; API and HTTPS clients come
	// from the production constructor and have no projected ServiceAccount files.
	socket := testPodResourcesSocket(t)
	resources := &podResourcesFixture{response: &v1.ListPodResourcesResponse{PodResources: []*v1.PodResources{{
		Name: pod.Name, Namespace: pod.Namespace,
		Containers: []*v1.ContainerResources{{
			Name: "workload",
			Devices: []*v1.ContainerDevices{{
				ResourceName: "nvidia.com/gpu", DeviceIds: []string{"GPU-test-1"},
			}},
		}},
	}}}}
	startPodResourcesFixture(t, socket, resources)
	mapper.kubeletGRPCClient, err = newKubeletGRPClient(t.Context(), socket)
	require.NoError(t, err)

	_, err = mapper.UpdatePodDevicesAnnotations()
	require.True(t, apierrors.IsForbidden(err), "identity without pod patch permission must be rejected: %v", err)
	grantTestPodPatch(t, admin, pod.Namespace, "metadata-test")
	require.Eventually(t, func() bool {
		_, err := mapper.UpdatePodDevicesAnnotations()
		return err == nil
	}, 10*time.Second, 50*time.Millisecond)

	current, err := admin.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	var annotation model.DeviceAnnotation
	require.NoError(t, json.Unmarshal([]byte(current.Annotations[model.PodDeviceAnnotationName]), &annotation))
	assert.Equal(t, []string{"GPU-test-1"}, annotation.Devices["nvidia.com/gpu"])
}
