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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

const (
	kubeSecurePort     = "10250"
	defaultKubeletHost = "localhost"
	kubeletHostEnvVar  = "KUBELET_HOST"
	bearerTokenPath    = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // not a credential
)

// client-go's retry.DefaultRetry is documented for resource-version conflicts and waits about
// 40ms in total, which is too short to outlast a rotated credential or a restarting kubelet.
// Five steps sleep four times, so these wait roughly 7.5s.
var defaultListPodsBackoff = wait.Backoff{
	Steps:    5,
	Duration: 500 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
}

// Ceiling on one whole ListPods call. The caller polls every 30s, so a call slower than this is
// already failing to keep up; bounding it turns that into a counted failure the poll threshold
// can absorb, rather than a poll that silently overruns its period.
const defaultListPodsTimeout = 20 * time.Second

type KubeletHTTPSClient interface {
	ListPods() ([]KubeletPod, error)
}

// KubeletPod is the part of a kubelet /pods entry that the mapper reads. Decoding skips every
// other field, so the size of a pod's spec costs no memory. Annotations holds only the device
// annotation: other annotations can be as large as the pod itself, for example kubectl's
// last-applied-configuration, which is a copy of it.
type KubeletPod struct {
	Name        string
	Namespace   string
	Annotations map[string]string
}

// kubeletPodJSON is the subset of the pod schema that decodePodList reads.
type kubeletPodJSON struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

type kubeletHTTPSClient struct {
	ctx context.Context

	httpRoundTripper http.RoundTripper
	listPodsURI      string
	listPodsBackoff  wait.Backoff
	listPodsTimeout  time.Duration
}

// NewKubeletHTTPSClient accepts at most one kubeconfig path for kubelet HTTPS access.
// An omitted or empty path retains the in-cluster endpoint, token, and TLS behavior.
// An explicit path must provide credentials and a verified HTTPS endpoint.
// Requests use ctx for cancellation throughout the client's lifetime.
func NewKubeletHTTPSClient(ctx context.Context, kubeconfigPath ...string) (KubeletHTTPSClient, error) {
	if len(kubeconfigPath) > 1 {
		return nil, fmt.Errorf("expected at most one kubelet kubeconfig path")
	}

	kubeletHost := os.Getenv(kubeletHostEnvVar)
	if kubeletHost == "" {
		kubeletHost = defaultKubeletHost
	}

	config := &rest.Config{
		Host: "https://" + net.JoinHostPort(kubeletHost, kubeSecurePort),
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: true, //nolint:gosec // preserve in-cluster kubelet certificate behavior
			},
			TLSHandshakeTimeout:   30 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		WrapTransport: transport.TokenSourceWrapTransport(projectedTokenFile(bearerTokenPath)),
	}

	if len(kubeconfigPath) == 1 && kubeconfigPath[0] != "" {
		var err error

		config, err = loadRESTConfig(kubeconfigPath[0])
		if err != nil {
			return nil, fmt.Errorf("load kubelet configuration: %w", err)
		}
	}

	roundTripper, err := rest.TransportFor(config)
	if err != nil {
		return nil, fmt.Errorf("create kubelet transport: %w", err)
	}

	return &kubeletHTTPSClient{
		ctx:              ctx,
		httpRoundTripper: roundTripper,
		listPodsURI:      strings.TrimRight(config.Host, "/") + "/pods",
		listPodsBackoff:  defaultListPodsBackoff,
		listPodsTimeout:  defaultListPodsTimeout,
	}, nil
}

// projectedTokenFile avoids client-go's token cache so a retry immediately sees a rotated
// ServiceAccount token. The transport owns header injection; no token is retained by the client.
type projectedTokenFile string

func (path projectedTokenFile) Token() (*oauth2.Token, error) {
	tokenBytes, err := os.ReadFile(string(path))
	if err != nil {
		return nil, fmt.Errorf("could not read service account token %q: %w", path, err)
	}

	return &oauth2.Token{AccessToken: strings.TrimSpace(string(tokenBytes))}, nil
}

/*
This function calls the /pods Kubelet endpoint. Explicit kubeconfigs verify TLS and use
client-go authentication. The legacy in-cluster mode described below is unchanged.

- Kubelet host: by default the client connects to localhost, which works when kubelet binds to 0.0.0.0. On clusters
where kubelet binds to the node's primary IP, set the KUBELET_HOST environment variable to the node's IP. The Helm
chart injects this via the Kubernetes Downward API (status.hostIP).

- Insecure TLS justification: by default, Kubelet serving certificates are signed by the same certificate authority as
the kube-apiserver. As a result, the CA mounted in the pod file system at file path
/var/run/secrets/kubernetes.io/serviceaccount/ca.crt can be used against either server. However, this server certificate
only has a valid SAN for the node's primary IP and not localhost. We skip certificate validation to handle both cases.
The metadata-collector already runs with HostNetwork=true.

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
func (client *kubeletHTTPSClient) ListPods() ([]KubeletPod, error) {
	// One deadline for the whole call: every attempt, its response-body read, and the sleeps
	// between them. Neither half is bounded otherwise. The transport's timeouts cover only the
	// header exchange, reading the body has no deadline of its own, and retry.OnError sleeps
	// through wait.ExponentialBackoff, which never consults a context. So a hung kubelet could
	// hold ListPods well past the caller's poll period.
	ctx, cancel := context.WithTimeout(client.ctx, client.listPodsTimeout)
	defer cancel()

	var pods []KubeletPod

	err := retry.OnError(client.listPodsBackoff, retriableUntil(ctx), func() error {
		var err error

		pods, err = client.fetchPods(ctx)

		return err
	})
	if err != nil {
		return nil, err
	}

	return pods, nil
}

// fetchPods performs one attempt at the /pods request. The transport supplies authentication.
// The body is decoded while it is read, so a malformed body fails the attempt and is retried
// like a failed read.
func (client *kubeletHTTPSClient) fetchPods(ctx context.Context) ([]KubeletPod, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.listPodsURI, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Accept", "application/json")

	resp, err := client.httpRoundTripper.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("got an error making HTTP request to /pods endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet := make([]byte, 512)
		n, _ := resp.Body.Read(snippet)

		if n > 0 {
			return nil, fmt.Errorf("got a non-200 response code from /pods endpoint: %d, body: %s",
				resp.StatusCode, string(snippet[:n]))
		}

		return nil, fmt.Errorf("got a non-200 response code from /pods endpoint: %d", resp.StatusCode)
	}

	pods, err := decodePodList(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("got an error decoding response from /pods endpoint: %w", err)
	}

	return pods, nil
}

// decodePodList reads a PodList one item at a time, so at most one pod's JSON is buffered.
// Decoding the whole list in one call would buffer the entire response first. It returns pods
// only for a complete list.
func decodePodList(body io.Reader) ([]KubeletPod, error) {
	decoder := json.NewDecoder(body)

	if err := expectDelim(decoder, '{'); err != nil {
		return nil, err
	}

	pods, err := decodePodListFields(decoder)
	if err != nil {
		return nil, err
	}

	if err := expectDelim(decoder, '}'); err != nil {
		return nil, err
	}

	if err := expectEnd(decoder); err != nil {
		return nil, err
	}

	return pods, nil
}

// decodePodListFields reads the PodList's fields, decoding items and skipping the rest.
func decodePodListFields(decoder *json.Decoder) ([]KubeletPod, error) {
	var pods []KubeletPod

	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("read pod list field: %w", unexpectedEOF(err))
		}

		if key != "items" {
			var skipped json.RawMessage
			if err := decoder.Decode(&skipped); err != nil {
				return nil, fmt.Errorf("read pod list field %v: %w", key, err)
			}

			continue
		}

		pods, err = decodePodItems(decoder)
		if err != nil {
			return nil, err
		}
	}

	return pods, nil
}

// decodePodItems reads the items array. A null array is an empty list, as json.Unmarshal reads it.
func decodePodItems(decoder *json.Decoder) ([]KubeletPod, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("read pod list items: %w", unexpectedEOF(err))
	}

	if token == nil {
		return nil, nil
	}

	if token != json.Delim('[') {
		return nil, fmt.Errorf("pod list items: expected an array, got %v", token)
	}

	var pods []KubeletPod

	for decoder.More() {
		var item kubeletPodJSON
		if err := decoder.Decode(&item); err != nil {
			return nil, fmt.Errorf("decode pod %d: %w", len(pods), err)
		}

		pods = append(pods, newKubeletPod(item.Metadata.Name, item.Metadata.Namespace, item.Metadata.Annotations))
	}

	if err := expectDelim(decoder, ']'); err != nil {
		return nil, err
	}

	return pods, nil
}

// newKubeletPod keeps only the device annotation. A present but empty annotation is kept,
// because the mapper removes an annotation only when it exists.
func newKubeletPod(name, namespace string, annotations map[string]string) KubeletPod {
	pod := KubeletPod{Name: name, Namespace: namespace}

	if value, ok := annotations[model.PodDeviceAnnotationName]; ok {
		pod.Annotations = map[string]string{model.PodDeviceAnnotationName: value}
	}

	return pod
}

func expectDelim(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read %q: %w", want, unexpectedEOF(err))
	}

	if token != want {
		return fmt.Errorf("expected %q, got %v", want, token)
	}

	return nil
}

// expectEnd rejects anything but whitespace after the pod list, as json.Unmarshal does.
func expectEnd(decoder *json.Decoder) error {
	_, err := decoder.Token()

	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return fmt.Errorf("read past the pod list: %w", err)
	default:
		return errors.New("unexpected data after the pod list")
	}
}

// unexpectedEOF reports a body that ends inside the pod list as truncated. At some positions
// the decoder's Token returns a plain io.EOF there, which reads as a clean end of input.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}

	return err
}

// retriableUntil retries any error until ctx is done. Returning false there is what stops
// retry.OnError sleeping past the deadline, since its backoff never consults a context.
func retriableUntil(ctx context.Context) func(error) bool {
	return func(_ error) bool {
		return ctx.Err() == nil
	}
}

// Still used by the gRPC client. Retrying every error is not right: a permanent fault such as a
// wrong bearerTokenPath is retried and then reported as though it were transient. Classifying
// retryable against permanent is worth doing, but it interacts with the caller's failure
// threshold, so it belongs in its own change.
func retryAllErrors(_ error) bool {
	return true
}
