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

package helpers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
)

const (
	// PlatformConnectorDeploymentName is the Deployment, Service, ServiceAccount
	// and ClusterRole name of the deployment platform connector (ADR 052).
	PlatformConnectorDeploymentName = "platform-connector-deployment"

	// PlatformConnectorDeploymentPodLabel selects the connector's replicas.
	PlatformConnectorDeploymentPodLabel = "app.kubernetes.io/name=" + PlatformConnectorDeploymentName

	// platformConnectorTLSVolume is the Deployment volume carrying the
	// cert-manager Secret with the serving certificate and its CA.
	platformConnectorTLSVolume = "grpc-tls"
	// platformConnectorConfigVolume is the Deployment volume carrying the
	// shared config.json ConfigMap.
	platformConnectorConfigVolume = "platform-connector-configmap"

	// PlatformConnectorStoreBatchesMetric counts the batches the store wrote
	// inside a request, by outcome: stored, or duplicate for a resend.
	PlatformConnectorStoreBatchesMetric = "platform_connector_store_batches_total"
	// PlatformConnectorBestEffortFailuresMetric counts what a best-effort
	// connector (node conditions, the gRPC sink) did not finish although the
	// batch was acknowledged, by connector and reason.
	PlatformConnectorBestEffortFailuresMetric = "platform_connector_best_effort_failures_total"
	// PlatformConnectorAuthViolationsMetric counts batches the node-binding
	// interceptor refused, by reason; a node-scoped caller naming another
	// node is node_mismatch.
	PlatformConnectorAuthViolationsMetric = "platform_connector_auth_violations_total"

	// IdempotencyKeyHeader is the gRPC metadata key the connector requires on
	// every batch.
	IdempotencyKeyHeader = "idempotency-key"

	platformConnectorRequestTimeout = 30 * time.Second
	boundTokenLifetimeSeconds       = int64(600)

	// probeComponentClass is the component class of the contract tests' events.
	probeComponentClass = "GPU"
)

// PlatformConnectorDeploymentFacts is what a client needs to reach the
// deployment platform connector the way the monitors do, read from the
// running Deployment so the tests follow the chart rather than restate it.
type PlatformConnectorDeploymentFacts struct {
	// Audience is the token audience the connector accepts, AuthAudience in the
	// shared config.json both roles read.
	Audience string
	// ServerName is the TLS name the serving certificate is issued for.
	ServerName string
	// CA is the PEM bundle that signed the serving certificate.
	CA []byte
	// GRPCPort and MetricsPort are the container ports named grpc and metrics.
	GRPCPort    int32
	MetricsPort int32
	// Replicas is the Deployment's desired replica count.
	Replicas int32
}

// readContainer picks the grpc and metrics ports out of one container of the
// Deployment.
func (f *PlatformConnectorDeploymentFacts) readContainer(container *v1.Container) {
	for _, port := range container.Ports {
		switch port.Name {
		case "grpc":
			f.GRPCPort = port.ContainerPort
		case "metrics":
			f.MetricsPort = port.ContainerPort
		}
	}
}

// readAudience reads AuthAudience from the config.json of the ConfigMap the
// Deployment mounts, the same key the DaemonSet reads.
func (f *PlatformConnectorDeploymentFacts) readAudience(
	ctx context.Context, t *testing.T, client klient.Client, deployment *appsv1.Deployment,
) {
	t.Helper()

	configMapName := ""

	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == platformConnectorConfigVolume && volume.ConfigMap != nil {
			configMapName = volume.ConfigMap.Name
		}
	}

	require.NotEmpty(t, configMapName,
		"the connector Deployment mounts no %s ConfigMap volume", platformConnectorConfigVolume)

	configMap := &v1.ConfigMap{}
	require.NoError(t, client.Resources().Get(ctx, configMapName, NVSentinelNamespace, configMap),
		"failed to get the %s ConfigMap", configMapName)

	raw := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(configMap.Data["config.json"]), &raw), "config.json of %s", configMapName)

	f.Audience, _ = raw["AuthAudience"].(string)
}

// PlatformConnectorDeploymentDeployed reports whether the run deployed the
// deployment platform connector (tilt with USE_DEPLOYMENT_PLATFORM_CONNECTOR=1).
func PlatformConnectorDeploymentDeployed(t *testing.T, client klient.Client) bool {
	t.Helper()

	err := client.Resources().Get(context.Background(), PlatformConnectorDeploymentName, NVSentinelNamespace,
		&appsv1.Deployment{})
	if apierrors.IsNotFound(err) {
		return false
	}

	require.NoError(t, err, "failed to look up the %s Deployment", PlatformConnectorDeploymentName)

	return true
}

// SkipWithoutPlatformConnectorDeployment skips a test that needs the deployment
// platform connector when the run deployed only the node-local DaemonSet.
func SkipWithoutPlatformConnectorDeployment(t *testing.T, client klient.Client) {
	t.Helper()

	if !PlatformConnectorDeploymentDeployed(t, client) {
		t.Skipf("the %s Deployment is not deployed in this run", PlatformConnectorDeploymentName)
	}
}

// GetPlatformConnectorDeploymentFacts reads the connector's audience, ports,
// TLS name and CA from the Deployment and the Secret it mounts.
func GetPlatformConnectorDeploymentFacts(
	ctx context.Context, t *testing.T, client klient.Client,
) *PlatformConnectorDeploymentFacts {
	t.Helper()

	deployment := &appsv1.Deployment{}
	require.NoError(t, client.Resources().Get(ctx, PlatformConnectorDeploymentName, NVSentinelNamespace, deployment),
		"failed to get the %s Deployment", PlatformConnectorDeploymentName)

	facts := &PlatformConnectorDeploymentFacts{
		ServerName: fmt.Sprintf("%s.%s.svc", PlatformConnectorDeploymentName, NVSentinelNamespace),
		Replicas:   1,
	}

	if deployment.Spec.Replicas != nil {
		facts.Replicas = *deployment.Spec.Replicas
	}

	for i := range deployment.Spec.Template.Spec.Containers {
		facts.readContainer(&deployment.Spec.Template.Spec.Containers[i])
	}

	require.NotZero(t, facts.GRPCPort, "the connector Deployment has no container port named grpc")
	require.NotZero(t, facts.MetricsPort, "the connector Deployment has no container port named metrics")
	facts.readAudience(ctx, t, client, deployment)
	require.NotEmpty(t, facts.Audience, "the connector's config.json sets no AuthAudience")

	secretName := ""

	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == platformConnectorTLSVolume && volume.Secret != nil {
			secretName = volume.Secret.SecretName
		}
	}

	require.NotEmpty(t, secretName,
		"the connector Deployment mounts no %s Secret volume; TLS is expected to be required", platformConnectorTLSVolume)

	secret := &v1.Secret{}
	require.NoError(t, client.Resources().Get(ctx, secretName, NVSentinelNamespace, secret),
		"failed to get the connector TLS Secret %s", secretName)

	facts.CA = secret.Data["ca.crt"]
	require.NotEmpty(t, facts.CA, "the connector TLS Secret %s has no ca.crt", secretName)

	return facts
}

// ListPlatformConnectorPods returns the connector's replicas, ready or not.
func ListPlatformConnectorPods(ctx context.Context, client klient.Client) ([]v1.Pod, error) {
	pods := &v1.PodList{}

	err := client.Resources(NVSentinelNamespace).List(ctx, pods, func(opts *metav1.ListOptions) {
		opts.LabelSelector = PlatformConnectorDeploymentPodLabel
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list %s pods: %w", PlatformConnectorDeploymentName, err)
	}

	return pods.Items, nil
}

// GetReadyPlatformConnectorPod waits for a ready connector replica and returns it.
func GetReadyPlatformConnectorPod(ctx context.Context, t *testing.T, client klient.Client) *v1.Pod {
	t.Helper()

	var ready *v1.Pod

	require.Eventually(t, func() bool {
		pods, err := ListPlatformConnectorPods(ctx, client)
		if err != nil {
			t.Logf("failed to list connector pods: %v", err)

			return false
		}

		for i := range pods {
			if isPodRunningAndReady(&pods[i]) {
				ready = &pods[i]

				return true
			}
		}

		return false
	}, EventuallyWaitTimeout, WaitInterval, "no ready %s replica", PlatformConnectorDeploymentName)

	return ready
}

// WaitForNoPlatformConnectorPods waits until the connector has no replicas
// left, so nothing can accept a batch.
func WaitForNoPlatformConnectorPods(ctx context.Context, t *testing.T, client klient.Client, timeout time.Duration) {
	t.Helper()

	require.Eventually(t, func() bool {
		pods, err := ListPlatformConnectorPods(ctx, client)
		if err != nil {
			t.Logf("failed to list connector pods: %v", err)

			return false
		}

		t.Logf("%d %s pod(s) still present", len(pods), PlatformConnectorDeploymentName)

		return len(pods) == 0
	}, timeout, WaitInterval, "%s pods should all be gone", PlatformConnectorDeploymentName)
}

// MintPodBoundToken requests a projected-style ServiceAccount token for the
// connector audience through the TokenRequest API. With pod set, the token is
// bound to that pod, which is what gives it a node claim; with pod nil it is
// an unbound token the connector must refuse.
func MintPodBoundToken(
	ctx context.Context, t *testing.T, restConfig *rest.Config, serviceAccount, audience string, pod *v1.Pod,
) string {
	t.Helper()

	clientset, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err, "failed to create clientset")

	expiration := boundTokenLifetimeSeconds
	request := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expiration,
		},
	}

	if pod != nil {
		request.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{
			Kind:       "Pod",
			APIVersion: "v1",
			Name:       pod.Name,
			UID:        pod.UID,
		}
	}

	response, err := clientset.CoreV1().ServiceAccounts(NVSentinelNamespace).
		CreateToken(ctx, serviceAccount, request, metav1.CreateOptions{})
	require.NoError(t, err, "failed to mint a token for ServiceAccount %s", serviceAccount)
	require.NotEmpty(t, response.Status.Token, "TokenRequest for %s returned an empty token", serviceAccount)

	return response.Status.Token
}

// PlatformConnectorConn is a client connection to one connector replica
// through a port-forward; Close releases both.
type PlatformConnectorConn struct {
	Client pb.PlatformConnectorClient
	Pod    *v1.Pod

	conn *grpc.ClientConn
	stop chan struct{}
}

// Close closes the gRPC connection and the port-forward behind it.
func (c *PlatformConnectorConn) Close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}

	if c.stop != nil {
		close(c.stop)
	}
}

// DialPlatformConnectorPod port-forwards localPort to one replica's gRPC port
// and dials it over TLS, trusting the chart's CA and expecting the Service's
// certificate name, exactly as a monitor does. With plaintext set the dial
// uses no TLS, to show the listener refuses it.
func DialPlatformConnectorPod(
	ctx context.Context, t *testing.T, restConfig *rest.Config, pod *v1.Pod,
	facts *PlatformConnectorDeploymentFacts, localPort int, plaintext bool,
) *PlatformConnectorConn {
	t.Helper()

	stop, ready := PortForwardPod(ctx, restConfig, pod.Namespace, pod.Name, localPort, int(facts.GRPCPort))

	select {
	case <-ready:
	case <-time.After(time.Minute):
		close(stop)
		require.FailNow(t, "port-forward to connector pod did not become ready", "pod %s", pod.Name)
	}

	creds := credentials.NewTLS(platformConnectorTLSConfig(t, facts))
	if plaintext {
		creds = insecure.NewCredentials()
	}

	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}

	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", localPort), opts...)
	if err != nil {
		close(stop)
		require.NoError(t, err, "failed to dial the connector through the port-forward")
	}

	return &PlatformConnectorConn{
		Client: pb.NewPlatformConnectorClient(conn),
		Pod:    pod,
		conn:   conn,
		stop:   stop,
	}
}

func platformConnectorTLSConfig(t *testing.T, facts *PlatformConnectorDeploymentFacts) *tls.Config {
	t.Helper()

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(facts.CA), "the connector CA bundle has no parsable certificate")

	return &tls.Config{
		RootCAs:    pool,
		ServerName: facts.ServerName,
		MinVersion: tls.VersionTLS12,
	}
}

// SendBatchToPlatformConnector sends one batch with the given bearer token
// and idempotency key; either may be empty to leave the header out. The
// request carries the monitors' 30 s deadline.
func SendBatchToPlatformConnector(
	ctx context.Context, client pb.PlatformConnectorClient, token, idempotencyKey string, batch *pb.HealthEvents,
) error {
	ctx, cancel := context.WithTimeout(ctx, platformConnectorRequestTimeout)
	defer cancel()

	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}

	if idempotencyKey != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, IdempotencyKeyHeader, idempotencyKey)
	}

	_, err := client.HealthEventOccurredV1(ctx, batch)

	return err
}

// NewPlatformConnectorBatch wraps one health event the way the monitors do.
func NewPlatformConnectorBatch(event *pb.HealthEvent) *pb.HealthEvents {
	return &pb.HealthEvents{Version: 1, Events: []*pb.HealthEvent{event}}
}

// NewProbeHealthEvent is a harmless health event for the connector contract
// tests: healthy, not fatal, GPU class, with a check name of the caller's
// choosing so nothing downstream matches a remediation rule.
func NewProbeHealthEvent(agent, nodeName, checkName, message string) *pb.HealthEvent {
	return &pb.HealthEvent{
		Version:           1,
		Agent:             agent,
		ComponentClass:    probeComponentClass,
		CheckName:         checkName,
		IsFatal:           false,
		IsHealthy:         true,
		Message:           message,
		RecommendedAction: pb.RecommendedAction_NONE,
		NodeName:          nodeName,
		EntitiesImpacted:  []*pb.Entity{},
	}
}

// ScrapePodMetrics reads a pod's Prometheus text exposition through the API
// server's pod proxy, so no port-forward is needed and the numbers are those
// of exactly that replica.
func ScrapePodMetrics(
	ctx context.Context, restConfig *rest.Config, namespace, podName string, port int32,
) (string, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	raw, err := clientset.CoreV1().RESTClient().Get().
		Namespace(namespace).
		Resource("pods").
		Name(fmt.Sprintf("%s:%d", podName, port)).
		SubResource("proxy").
		Suffix("metrics").
		DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to scrape %s/%s:%d: %w", namespace, podName, port, err)
	}

	return string(raw), nil
}

// MetricSampleValue returns the value of the sample of family `name` whose
// labels include every pair in `labels`: a counter's value, a gauge's value
// or a histogram's sample count. Missing family or sample counts as zero, as
// a fresh replica exposes nothing until the first event.
func MetricSampleValue(t *testing.T, exposition, name string, labels map[string]string) float64 {
	t.Helper()

	// The zero-value parser has no name validation scheme in this version of
	// prometheus/common and panics on the first metric family.
	parser := expfmt.NewTextParser(model.UTF8Validation)

	families, err := parser.TextToMetricFamilies(strings.NewReader(exposition))
	require.NoError(t, err, "failed to parse the metrics exposition")

	family, ok := families[name]
	if !ok {
		return 0
	}

	total := 0.0

	for _, metric := range family.GetMetric() {
		if !metricHasLabels(metric.GetLabel(), labels) {
			continue
		}

		switch {
		case metric.GetHistogram() != nil:
			total += float64(metric.GetHistogram().GetSampleCount())
		case metric.GetCounter() != nil:
			total += metric.GetCounter().GetValue()
		case metric.GetGauge() != nil:
			total += metric.GetGauge().GetValue()
		}
	}

	return total
}

func metricHasLabels(pairs []*dto.LabelPair, wanted map[string]string) bool {
	for key, value := range wanted {
		found := false

		for _, pair := range pairs {
			if pair.GetName() == key && pair.GetValue() == value {
				found = true

				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

// SumPlatformConnectorMetric adds the sample described by name and labels
// over every connector replica; a replica that cannot be scraped (for
// example one still starting) counts as zero and is logged.
func SumPlatformConnectorMetric(
	ctx context.Context, t *testing.T, restConfig *rest.Config, client klient.Client,
	metricsPort int32, name string, labels map[string]string,
) float64 {
	t.Helper()

	pods, err := ListPlatformConnectorPods(ctx, client)
	require.NoError(t, err)

	total := 0.0

	for i := range pods {
		exposition, err := ScrapePodMetrics(ctx, restConfig, pods[i].Namespace, pods[i].Name, metricsPort)
		if err != nil {
			t.Logf("skipping replica %s: %v", pods[i].Name, err)

			continue
		}

		total += MetricSampleValue(t, exposition, name, labels)
	}

	return total
}

// PlatformConnectorPodMetric reads one sample from one replica.
func PlatformConnectorPodMetric(
	ctx context.Context, t *testing.T, restConfig *rest.Config, pod *v1.Pod,
	metricsPort int32, name string, labels map[string]string,
) float64 {
	t.Helper()

	exposition, err := ScrapePodMetrics(ctx, restConfig, pod.Namespace, pod.Name, metricsPort)
	require.NoError(t, err, "failed to scrape replica %s", pod.Name)

	return MetricSampleValue(t, exposition, name, labels)
}

// GetClusterRoleRules returns a copy of the named ClusterRole's rules.
func GetClusterRoleRules(ctx context.Context, client klient.Client, name string) ([]rbacv1.PolicyRule, error) {
	role := &rbacv1.ClusterRole{}
	if err := client.Resources().Get(ctx, name, "", role); err != nil {
		return nil, fmt.Errorf("failed to get ClusterRole %s: %w", name, err)
	}

	rules := make([]rbacv1.PolicyRule, len(role.Rules))
	for i := range role.Rules {
		rules[i] = *role.Rules[i].DeepCopy()
	}

	return rules, nil
}

// SetClusterRoleRules replaces the named ClusterRole's rules.
func SetClusterRoleRules(ctx context.Context, client klient.Client, name string, rules []rbacv1.PolicyRule) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		role := &rbacv1.ClusterRole{}
		if err := client.Resources().Get(ctx, name, "", role); err != nil {
			return fmt.Errorf("failed to get ClusterRole %s: %w", name, err)
		}

		role.Rules = rules

		return client.Resources().Update(ctx, role)
	})
}

// ReadOnlyNodeRules keeps every rule of the connector's ClusterRole except
// the ones that let it write nodes, so the store keeps working while node
// condition updates are forbidden.
func ReadOnlyNodeRules(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	readOnly := make([]rbacv1.PolicyRule, 0, len(rules))

	for _, rule := range rules {
		copied := *rule.DeepCopy()

		if touchesNodes(copied.Resources) {
			copied.Verbs = readOnlyVerbs(copied.Verbs)
			if len(copied.Verbs) == 0 {
				continue
			}
		}

		readOnly = append(readOnly, copied)
	}

	return readOnly
}

func touchesNodes(resources []string) bool {
	for _, resource := range resources {
		if resource == "nodes" || strings.HasPrefix(resource, "nodes/") {
			return true
		}
	}

	return false
}

func readOnlyVerbs(verbs []string) []string {
	kept := make([]string, 0, len(verbs))

	for _, verb := range verbs {
		if verb == "get" || verb == "list" || verb == "watch" {
			kept = append(kept, verb)
		}
	}

	return kept
}

// PodRestartCount sums the restart counts of a pod's containers.
func PodRestartCount(ctx context.Context, client klient.Client, namespace, name string) (int32, error) {
	pod := &v1.Pod{}
	if err := client.Resources().Get(ctx, name, namespace, pod); err != nil {
		return 0, fmt.Errorf("failed to get pod %s/%s: %w", namespace, name, err)
	}

	var restarts int32
	for _, status := range pod.Status.ContainerStatuses {
		restarts += status.RestartCount
	}

	return restarts, nil
}
