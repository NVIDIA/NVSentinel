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

// Package helpers provides test utilities for the NVSentinel E2E test suite.
//
// IMPORTANT — MongoDB direct-access helpers:
// The helpers in this file (ExecMongosh, InsertStaleResumeToken, DeleteResumeToken, etc.)
// exec mongosh directly inside the MongoDB pod to manipulate resume tokens. This approach
// is specific to the stale-resume-token recovery test (TestStaleResumeTokenRecovery) and
// should NOT be used as a general pattern for other tests. Normal E2E tests should interact
// with MongoDB indirectly through the application's APIs (e.g., sending health events via
// the simple-health-client, checking node labels/annotations via the Kubernetes API).
// Direct MongoDB manipulation bypasses the application layer and can create state that is
// inconsistent with what the application expects.
package helpers

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/klient"
)

const (
	MongoDBDatabase        = "HealthEventsDatabase"
	MongoDBTokenCollection = "ResumeTokens"
)

type mongoDBFlavor struct {
	LabelSelector string
	ContainerName string
	ServiceName   string
}

var (
	bitnamiFlavor = mongoDBFlavor{
		LabelSelector: "app.kubernetes.io/component=mongodb",
		ContainerName: "mongodb",
		ServiceName:   "mongodb-headless",
	}
	perconaFlavor = mongoDBFlavor{
		LabelSelector: "app.kubernetes.io/component=mongod",
		ContainerName: "mongod",
		ServiceName:   "mongodb-rs0",
	}
)

// GetMongoDBPrimaryPodName returns the name of the writable primary MongoDB pod.
// It auto-detects Bitnami vs Percona by trying both label selectors, then picks
// the pod whose ordinal matches the configured primary (ordinal 0 for Bitnami,
// highest-priority member for Percona based on replsetOverrides).
// In a fresh 3-node replica set, ordinal 0 is the primary in both flavors.
func GetMongoDBPrimaryPodName(
	ctx context.Context, t *testing.T, client klient.Client,
) string {
	t.Helper()

	for _, flavor := range []mongoDBFlavor{perconaFlavor, bitnamiFlavor} {
		pods := &v1.PodList{}
		err := client.Resources().List(ctx, pods, func(opts *metav1.ListOptions) {
			opts.LabelSelector = flavor.LabelSelector
		})
		require.NoError(t, err, "failed to list MongoDB pods with selector %s", flavor.LabelSelector)

		var runningPods []v1.Pod

		for _, pod := range pods.Items {
			if pod.Namespace == NVSentinelNamespace && pod.Status.Phase == v1.PodRunning {
				runningPods = append(runningPods, pod)
			}
		}

		if len(runningPods) == 0 {
			continue
		}

		// Prefer ordinal-0 pod (the configured primary in both Bitnami and
		// Percona deployments with replsetOverrides giving it highest priority).
		for _, pod := range runningPods {
			if strings.HasSuffix(pod.Name, "-0") {
				t.Logf("Found primary MongoDB pod: %s (flavor: %s)", pod.Name, flavor.ContainerName)
				return pod.Name
			}
		}

		// Fallback: return any running pod (shouldn't happen with 3-node RS).
		t.Logf("Found running MongoDB pod (no -0 ordinal): %s (flavor: %s)", runningPods[0].Name, flavor.ContainerName)

		return runningPods[0].Name
	}

	require.Fail(t, "no running MongoDB pod found in namespace %s", NVSentinelNamespace)

	return ""
}

// detectMongoDBFlavor determines whether the given pod belongs to a Bitnami or
// Percona deployment by checking its labels. Returns the matching flavor.
func detectMongoDBFlavor(ctx context.Context, t *testing.T, client klient.Client, podName string) mongoDBFlavor {
	t.Helper()

	pods := &v1.PodList{}
	err := client.Resources().List(ctx, pods, func(opts *metav1.ListOptions) {
		opts.LabelSelector = perconaFlavor.LabelSelector
	})
	require.NoError(t, err, "failed to list Percona pods")

	for _, pod := range pods.Items {
		if pod.Name == podName && pod.Namespace == NVSentinelNamespace {
			t.Logf("Detected Percona MongoDB flavor for pod %s", podName)
			return perconaFlavor
		}
	}

	t.Logf("Detected Bitnami MongoDB flavor for pod %s", podName)

	return bitnamiFlavor
}

// readPerconaCredentials reads the databaseAdmin username and password from the
// Percona operator's internal-mongodb-users Secret. These are not injected as
// env vars into the mongod container, so we must fetch them via the K8s API.
func readPerconaCredentials(ctx context.Context, t *testing.T, restConfig *rest.Config) (string, string) {
	t.Helper()

	clientset, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err, "failed to create kubernetes clientset")

	secret, err := clientset.CoreV1().Secrets(NVSentinelNamespace).Get(ctx, "internal-mongodb-users", metav1.GetOptions{})
	require.NoError(t, err, "failed to read internal-mongodb-users secret")

	user := string(secret.Data["MONGODB_DATABASE_ADMIN_USER"])
	pass := string(secret.Data["MONGODB_DATABASE_ADMIN_PASSWORD"])

	require.NotEmpty(t, user, "MONGODB_DATABASE_ADMIN_USER not found in secret")
	require.NotEmpty(t, pass, "MONGODB_DATABASE_ADMIN_PASSWORD not found in secret")

	return user, pass
}

// buildMongoshCommand constructs a mongosh command against the headless service
// hostname. TLS and auth flags are auto-detected at exec time inside the pod:
//   - Bitnami: TLS via /certs/mongodb.pem, auth via MONGODB_ROOT_PASSWORD (user: root)
//   - Percona: mTLS via /etc/mongodb-ssl-internal/ certs (operator-managed), auth
//     via credentials read from the internal-mongodb-users Secret
//
// IMPORTANT: All JavaScript passed to jsEval MUST use double quotes for strings
// (not single quotes), because the --eval argument is wrapped in single quotes
// to prevent shell expansion of $ characters (e.g. $set, $external).
func buildMongoshCommand(mongoPod string, flavor mongoDBFlavor, perconaUser, perconaPass, jsEval string) []string {
	host := fmt.Sprintf("%s.%s.%s.svc.cluster.local", mongoPod, flavor.ServiceName, NVSentinelNamespace)

	if flavor.ContainerName == "mongod" {
		return buildPerconaMongoshCommand(host, perconaUser, perconaPass, jsEval)
	}

	return buildBitnamiMongoshCommand(host, jsEval)
}

// buildPerconaMongoshCommand constructs a shell command for Percona mongod pods.
// Credentials are base64-encoded to safely pass through shell without metacharacter
// issues (passwords may contain $, spaces, backticks, etc.). Variables are always
// expanded inside double quotes to prevent word-splitting.
//
// TLS: Percona requires mTLS (client must present a certificate). The operator
// mounts internal certs at /etc/mongodb-ssl-internal/. We combine tls.crt and
// tls.key into a PEM file for mongosh. If the cert dir is absent (TLS disabled),
// we fall back to a plain connection.
func buildPerconaMongoshCommand(host, user, pass, jsEval string) []string {
	userB64 := base64.StdEncoding.EncodeToString([]byte(user))
	passB64 := base64.StdEncoding.EncodeToString([]byte(pass))

	//nolint:lll // shell one-liner is clearer without artificial line breaks
	script := fmt.Sprintf(
		`PERCONA_USER="$(printf '%%s' '%s' | base64 -d)"; `+
			`PERCONA_PASS="$(printf '%%s' '%s' | base64 -d)"; `+
			`TLS_ARGS=""; `+
			`if [ -f /etc/mongodb-ssl-internal/tls.crt ]; then `+
			`cat /etc/mongodb-ssl-internal/tls.crt /etc/mongodb-ssl-internal/tls.key > /tmp/mongod-test.pem; `+
			`TLS_ARGS="--tls --tlsCAFile /etc/mongodb-ssl-internal/ca.crt --tlsCertificateKeyFile /tmp/mongod-test.pem"; fi; `+
			`mongosh --quiet --host %s $TLS_ARGS `+
			`--username "$PERCONA_USER" --password "$PERCONA_PASS" `+
			`--authenticationDatabase admin --authenticationMechanism SCRAM-SHA-256 `+
			`--eval '%s'`,
		userB64, passB64, host, jsEval,
	)

	return []string{"/bin/sh", "-c", script}
}

// buildBitnamiMongoshCommand constructs a shell command for Bitnami mongodb pods.
// TLS and auth flags are auto-detected from env vars and cert files inside the pod.
// Auth credentials are passed via properly quoted variable references to prevent
// word-splitting on passwords containing spaces or shell metacharacters.
func buildBitnamiMongoshCommand(host, jsEval string) []string {
	//nolint:lll // shell one-liner is clearer without artificial line breaks
	script := fmt.Sprintf(
		`TLS_ARGS=""; `+
			`if [ -f /certs/mongodb.pem ]; then `+
			`TLS_ARGS="--tls --tlsCAFile /certs/mongodb-ca-cert `+
			`--tlsCertificateKeyFile /certs/mongodb.pem"; fi; `+
			`AUTH=""; `+
			`if [ -n "$MONGODB_ROOT_PASSWORD" ]; then AUTH=1; fi; `+
			`mongosh --quiet `+
			`--host %s `+
			`$TLS_ARGS `+
			`${AUTH:+--username root --password "$MONGODB_ROOT_PASSWORD" `+
			`--authenticationDatabase admin `+
			`--authenticationMechanism SCRAM-SHA-256} `+
			`--eval '%s'`,
		host, jsEval,
	)

	return []string{"/bin/sh", "-c", script}
}

// ExecMongosh runs a JavaScript expression inside a MongoDB pod via mongosh.
// Returns stdout and stderr from the command execution.
// All JS strings in the eval expression MUST use double quotes (not single quotes).
func ExecMongosh(
	ctx context.Context, t *testing.T, restConfig *rest.Config, client klient.Client, mongoPod, js string,
) (string, string) {
	t.Helper()

	flavor := detectMongoDBFlavor(ctx, t, client, mongoPod)

	var perconaUser, perconaPass string
	if flavor.ContainerName == "mongod" {
		perconaUser, perconaPass = readPerconaCredentials(ctx, t, restConfig)
	}

	cmd := buildMongoshCommand(mongoPod, flavor, perconaUser, perconaPass, js)
	stdout, stderr, err := ExecInPod(ctx, restConfig, NVSentinelNamespace, mongoPod, flavor.ContainerName, cmd)
	require.NoError(t, err, "mongosh exec failed: stdout=%s stderr=%s", stdout, stderr)

	return stdout, stderr
}

// StaleTokenMarker is the _data value inserted by InsertStaleResumeToken.
// It is not valid hex, causing MongoDB to return FailedToParse (error code 9) on the
// Watch() call. Tests check for this string to verify the recovery path deleted the token.
const StaleTokenMarker = "INVALID_STALE_TOKEN"

// InsertStaleResumeToken inserts an invalid resume token for the given clientName into
// the ResumeTokens collection. The token's _data is not valid hex, causing MongoDB to
// return FailedToParse (error code 9) on the Watch() call. This works regardless of
// oplog state.
func InsertStaleResumeToken(
	ctx context.Context, t *testing.T, restConfig *rest.Config, client klient.Client, mongoPod, clientName string,
) {
	t.Helper()
	t.Logf("Inserting stale resume token for client %q into MongoDB", clientName)

	js := fmt.Sprintf(`
		db = db.getSiblingDB("%s");
		db.%s.updateOne(
			{ clientName: "%s" },
			{ $set: { clientName: "%s", resumeToken: { _data: "%s" } } },
			{ upsert: true }
		);
		let saved = db.%s.findOne({ clientName: "%s" });
		printjson(saved);
		print("Stale resume token inserted for %s");
	`, MongoDBDatabase,
		MongoDBTokenCollection, clientName, clientName, StaleTokenMarker,
		MongoDBTokenCollection, clientName, clientName)

	stdout, _ := ExecMongosh(ctx, t, restConfig, client, mongoPod, js)
	t.Logf("InsertStaleResumeToken output: %s", strings.TrimSpace(stdout))
}

// DeleteResumeToken removes the resume token for the given clientName from MongoDB.
func DeleteResumeToken(
	ctx context.Context, t *testing.T, restConfig *rest.Config, client klient.Client, mongoPod, clientName string,
) {
	t.Helper()
	t.Logf("Deleting resume token for client %q from MongoDB", clientName)

	js := fmt.Sprintf(`
		db = db.getSiblingDB("%s");
		result = db.%s.deleteOne({ clientName: "%s" });
		print("Deleted " + result.deletedCount + " resume token(s) for %s");
	`, MongoDBDatabase, MongoDBTokenCollection, clientName, clientName)

	stdout, _ := ExecMongosh(ctx, t, restConfig, client, mongoPod, js)
	t.Logf("DeleteResumeToken output: %s", strings.TrimSpace(stdout))
}

// GetResumeTokenDoc returns the resume token document for the given clientName, or empty string if not found.
func GetResumeTokenDoc(
	ctx context.Context, t *testing.T, restConfig *rest.Config, client klient.Client, mongoPod, clientName string,
) string {
	t.Helper()

	js := fmt.Sprintf(`
		db = db.getSiblingDB("%s");
		doc = db.%s.findOne({ clientName: "%s" });
		if (doc) { printjson(doc); } else { print("NOT_FOUND"); }
	`, MongoDBDatabase, MongoDBTokenCollection, clientName)

	stdout, _ := ExecMongosh(ctx, t, restConfig, client, mongoPod, js)

	return strings.TrimSpace(stdout)
}
