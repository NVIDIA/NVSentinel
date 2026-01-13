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

package mapper

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
)

const (
	kubeSecurePort      = "10250"
	kubeletHostName     = "localhost"
	bearerTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec
	listPodsURLTemplate = "https://%s:%s/pods"
)

type KubeletHTTPSClient interface {
	ListPods() ([]corev1.Pod, error)
}

type kubeletHTTPSClient struct {
	ctx context.Context

	httpRoundTripper http.RoundTripper

	// takes precedence over bearerTokenPath which will be dynamically loaded on every request, used for testing
	staticBearerToken string
	bearerTokenPath   string
	listPodsURI       string
}

func NewKubeletHTTPSClient(ctx context.Context) (KubeletHTTPSClient, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec
		},
		TLSHandshakeTimeout:   30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	return &kubeletHTTPSClient{
		ctx:              ctx,
		httpRoundTripper: transport,
		bearerTokenPath:  bearerTokenPath,
		listPodsURI:      fmt.Sprintf(listPodsURLTemplate, kubeletHostName, kubeSecurePort),
	}, nil
}

/*
This function calls the /pods Kubelet endpoint on localhost skipping Kubelet server certificate validation for TLS
while passing a service account token for authN and authZ.

- Insecure TLS justification: by default, Kubelet serving certificates are signed by the same certificate authority as
the kube-apiserver. As a result, the CA mounted in the pod file system at file path
/var/run/secrets/kubernetes.io/serviceaccount/ca.crt can be used against either server. However, this server certificate
only has a valid SAN for the node's primary IP and not localhost. To prevent needing to lookup the node's primary IP
via the K8s API or leveraging a HostPath volume, we will call the server listening on localhost and skip certificate
validation. The metadata-collector already runs with HostNetwork=true so the localhost interface will match the same one
that the Kubelet is serving on.

- Kubelet AuthN + AuthZ: Kubelet's can optionally enabled authentication with a bearer token that is validated via a
TokenReview and authorization that is validated via a SubjectAccessReview. To ensure that our metadata-collector pod
can successfully pass AuthN + AuthZ, we will pass the pod's SA token mounted in the pod at
/var/run/secrets/kubernetes.io/serviceaccount/token. The service account for this component is bound to a cluster role
which grants GET permission against the nodes/proxy resource (in addition to the patch pod permissions required).

In summary, we require:
- metadata-collector pods run with GET permission on nodes/proxy
- metadata-collector pods run with HostNetwork=true and skip Kubelet server certificate validation on localhost

Example for how to make an equivalent request via CLI:
curl -k -H "Authorization: Bearer $TOKEN" https://localhost:10250/pods
*/
func (client *kubeletHTTPSClient) ListPods() ([]corev1.Pod, error) {
	// We should read the token file on every request to prevent caching a stale service account token rotated
	// via a projected volume.
	token := client.staticBearerToken
	if len(token) == 0 {
		tokenBytes, err := os.ReadFile(client.bearerTokenPath)
		if err != nil {
			return nil, err
		}

		token = string(tokenBytes)
	}

	req, err := http.NewRequestWithContext(client.ctx, "GET", client.listPodsURI, nil)
	if err != nil {
		return nil, err
	}

	var resp *http.Response

	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Add("Accept", "application/json")

	err = retry.OnError(retry.DefaultRetry, retryAllErrors, func() error {
		resp, err = client.httpRoundTripper.RoundTrip(req) //nolint:bodyclose // response body is closed outside OnError
		if err != nil {
			return fmt.Errorf("got an error making HTTP request to /pods endpoint: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("got a non-200 response code from /pods endpoint: %d", resp.StatusCode)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	podBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("got an error reading response body from /pods endpoint: %w", err)
	}

	pods := corev1.PodList{}

	err = json.Unmarshal(podBytes, &pods)
	if err != nil {
		return nil, fmt.Errorf("got an error unmarshalling response from /pods: %w", err)
	}

	return pods.Items, nil
}

func retryAllErrors(_ error) bool {
	return true
}
