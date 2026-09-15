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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

const (
	podJson = `
{
  "kind": "PodList",
  "apiVersion": "v1",
  "metadata": {},
  "items": [
    {
      "metadata": {
        "name": "gpu-job-xkbmx",
        "generateName": "gpu-job-",
        "namespace": "default",
        "uid": "d54d2cf1-87fc-400f-b4b4-e7f144f1a537",
        "resourceVersion": "61533238",
        "creationTimestamp": "2025-12-05T22:32:47Z",
        "labels": {
          "batch.kubernetes.io/controller-uid": "9ac18b61-8544-4b44-b33c-6e1cf29300b2",
          "batch.kubernetes.io/job-name": "gpu-job",
          "controller-uid": "9ac18b61-8544-4b44-b33c-6e1cf29300b2",
          "job-name": "gpu-job"
        },
	    "annotations": {
		  "k8s.v1.cni.cncf.io/network-status": "[{\n    \"name\": \"cbr0\",\n    \"interface\": \"eth0\",\n    \"ips\": [\n        \"10.244.6.41\"\n    ],\n    \"mac\": \"0e:dd:23:c6:2a:c8\",\n    \"default\": true,\n    \"dns\": {},\n    \"gateway\": [\n        \"10.244.6.1\"\n    ]\n}]",
          "dgxc.nvidia.com/devices": "{\"devices\":{\"nvidia.com/gpu\":[\"GPU-455d8f70-2051-db6c-0430-ffc457bff834\"]}}"
	    },
        "ownerReferences": [
          {
            "apiVersion": "batch/v1",
            "kind": "Job",
            "name": "gpu-job",
            "uid": "9ac18b61-8544-4b44-b33c-6e1cf29300b2",
            "controller": true,
            "blockOwnerDeletion": true
          }
        ],
        "finalizers": [
          "batch.kubernetes.io/job-tracking"
        ]
      },
      "spec": {
        "volumes": [
          {
            "name": "kube-api-access-zvpq9",
            "projected": {
              "sources": [
                {
                  "serviceAccountToken": {
                    "expirationSeconds": 3607,
                    "path": "token"
                  }
                },
                {
                  "configMap": {
                    "name": "kube-root-ca.crt",
                    "items": [
                      {
                        "key": "ca.crt",
                        "path": "ca.crt"
                      }
                    ]
                  }
                },
                {
                  "downwardAPI": {
                    "items": [
                      {
                        "path": "namespace",
                        "fieldRef": {
                          "apiVersion": "v1",
                          "fieldPath": "metadata.namespace"
                        }
                      }
                    ]
                  }
                }
              ],
              "defaultMode": 420
            }
          }
        ],
        "containers": [
          {
            "name": "gpu-container",
            "image": "nvidia/cuda:12.3.2-base-ubuntu22.04",
            "command": [
              "sleep",
              "infinity"
            ],
            "resources": {
              "limits": {
                "nvidia.com/gpu": "1"
              },
              "requests": {
                "nvidia.com/gpu": "1"
              }
            },
            "volumeMounts": [
              {
                "name": "kube-api-access-zvpq9",
                "readOnly": true,
                "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount"
              }
            ],
            "terminationMessagePath": "/dev/termination-log",
            "terminationMessagePolicy": "File",
            "imagePullPolicy": "IfNotPresent"
          }
        ],
        "restartPolicy": "Never",
        "terminationGracePeriodSeconds": 30,
        "dnsPolicy": "ClusterFirst",
        "serviceAccountName": "default",
        "serviceAccount": "default",
        "nodeName": "10.0.15.229",
        "securityContext": {},
        "schedulerName": "default-scheduler",
        "tolerations": [
          {
            "key": "nvidia.com/gpu",
            "operator": "Equal",
            "value": "present",
            "effect": "NoSchedule"
          },
          {
            "key": "dedicated",
            "operator": "Equal",
            "value": "user-workload",
            "effect": "NoExecute"
          },
          {
            "key": "node.kubernetes.io/not-ready",
            "operator": "Exists",
            "effect": "NoExecute",
            "tolerationSeconds": 300
          },
          {
            "key": "node.kubernetes.io/unreachable",
            "operator": "Exists",
            "effect": "NoExecute",
            "tolerationSeconds": 300
          },
          {
            "key": "nvidia.com/gpu",
            "operator": "Exists",
            "effect": "NoSchedule"
          }
        ],
        "priority": 0,
        "enableServiceLinks": true,
        "preemptionPolicy": "PreemptLowerPriority"
      },
      "status": {
        "phase": "Running",
        "conditions": [
          {
            "type": "PodReadyToStartContainers",
            "status": "True",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-12-05T22:32:52Z"
          },
          {
            "type": "Initialized",
            "status": "True",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-12-05T22:32:51Z"
          },
          {
            "type": "Ready",
            "status": "True",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-12-05T22:32:52Z"
          },
          {
            "type": "ContainersReady",
            "status": "True",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-12-05T22:32:52Z"
          },
          {
            "type": "PodScheduled",
            "status": "True",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-12-05T22:32:47Z"
          }
        ],
        "hostIP": "10.0.15.229",
        "hostIPs": [
          {
            "ip": "10.0.15.229"
          }
        ],
        "podIP": "10.244.6.41",
        "podIPs": [
          {
            "ip": "10.244.6.41"
          }
        ],
        "startTime": "2025-12-05T22:32:51Z",
        "containerStatuses": [
          {
            "name": "gpu-container",
            "state": {
              "running": {
                "startedAt": "2025-12-05T22:32:52Z"
              }
            },
            "lastState": {},
            "ready": true,
            "restartCount": 0,
            "image": "docker.io/nvidia/cuda:12.3.2-base-ubuntu22.04",
            "imageID": "docker.io/nvidia/cuda@sha256:0dacc5e3c29a9a8233121a1aec8b7a023c727bed2cc235d4d5a2575403a13993",
            "containerID": "cri-o://1767d1fd8561642f818e16f411e6b1c61b77caabfe848266f2a44685eb782016",
            "started": true,
            "volumeMounts": [
              {
                "name": "kube-api-access-zvpq9",
                "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount",
                "readOnly": true,
                "recursiveReadOnly": "Disabled"
              }
            ]
          }
        ],
        "qosClass": "BestEffort"
      }
    },
    {
      "metadata": {
        "name": "node-debugger-10.0.12.244-2x8vm",
        "namespace": "default",
        "uid": "28e1ce73-b064-4a6e-ab07-0396dbc4b380",
        "resourceVersion": "43760925",
        "creationTimestamp": "2025-11-12T17:53:09Z",
        "labels": {
          "app.kubernetes.io/managed-by": "kubectl-debug"
        }
      },
      "spec": {
        "volumes": [
          {
            "name": "host-root",
            "hostPath": {
              "path": "/",
              "type": ""
            }
          },
          {
            "name": "kube-api-access-7jg5q",
            "projected": {
              "sources": [
                {
                  "serviceAccountToken": {
                    "expirationSeconds": 3607,
                    "path": "token"
                  }
                },
                {
                  "configMap": {
                    "name": "kube-root-ca.crt",
                    "items": [
                      {
                        "key": "ca.crt",
                        "path": "ca.crt"
                      }
                    ]
                  }
                },
                {
                  "downwardAPI": {
                    "items": [
                      {
                        "path": "namespace",
                        "fieldRef": {
                          "apiVersion": "v1",
                          "fieldPath": "metadata.namespace"
                        }
                      }
                    ]
                  }
                }
              ],
              "defaultMode": 420
            }
          }
        ],
        "containers": [
          {
            "name": "debugger",
            "image": "busybox",
            "command": [
              "sh"
            ],
            "resources": {},
            "volumeMounts": [
              {
                "name": "host-root",
                "mountPath": "/host"
              },
              {
                "name": "kube-api-access-7jg5q",
                "readOnly": true,
                "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount"
              }
            ],
            "terminationMessagePath": "/dev/termination-log",
            "terminationMessagePolicy": "File",
            "imagePullPolicy": "Always",
            "stdin": true,
            "tty": true
          }
        ],
        "restartPolicy": "Never",
        "terminationGracePeriodSeconds": 30,
        "dnsPolicy": "ClusterFirst",
        "serviceAccountName": "default",
        "serviceAccount": "default",
        "nodeName": "10.0.12.244",
        "hostNetwork": true,
        "hostPID": true,
        "hostIPC": true,
        "securityContext": {},
        "schedulerName": "default-scheduler",
        "tolerations": [
          {
            "operator": "Exists"
          }
        ],
        "priority": 0,
        "enableServiceLinks": true,
        "preemptionPolicy": "PreemptLowerPriority"
      },
      "status": {
        "phase": "Succeeded",
        "conditions": [
          {
            "type": "PodReadyToStartContainers",
            "status": "False",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-11-12T17:54:36Z"
          },
          {
            "type": "Initialized",
            "status": "True",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-11-12T17:53:09Z",
            "reason": "PodCompleted"
          },
          {
            "type": "Ready",
            "status": "False",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-11-12T17:54:35Z",
            "reason": "PodCompleted"
          },
          {
            "type": "ContainersReady",
            "status": "False",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-11-12T17:54:35Z",
            "reason": "PodCompleted"
          },
          {
            "type": "PodScheduled",
            "status": "True",
            "lastProbeTime": null,
            "lastTransitionTime": "2025-11-12T17:53:09Z"
          }
        ],
        "hostIP": "10.0.12.244",
        "hostIPs": [
          {
            "ip": "10.0.12.244"
          }
        ],
        "podIP": "10.0.12.244",
        "podIPs": [
          {
            "ip": "10.0.12.244"
          }
        ],
        "startTime": "2025-11-12T17:53:09Z",
        "containerStatuses": [
          {
            "name": "debugger",
            "state": {
              "terminated": {
                "exitCode": 0,
                "reason": "Completed",
                "startedAt": "2025-11-12T17:53:10Z",
                "finishedAt": "2025-11-12T17:54:35Z",
                "containerID": "cri-o://ae3d47d68b827997ff77097f1dc1f63df19ddf706512043c356d20efa5b2463a"
              }
            },
            "lastState": {},
            "ready": false,
            "restartCount": 0,
            "image": "docker.io/library/busybox:latest",
            "imageID": "docker.io/library/busybox@sha256:870e815c3a50dd0f6b40efddb319c72c32c3ee340b5a3e8945904232ccd12f44",
            "containerID": "cri-o://ae3d47d68b827997ff77097f1dc1f63df19ddf706512043c356d20efa5b2463a",
            "started": false,
            "volumeMounts": [
              {
                "name": "host-root",
                "mountPath": "/host"
              },
              {
                "name": "kube-api-access-7jg5q",
                "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount",
                "readOnly": true,
                "recursiveReadOnly": "Disabled"
              }
            ]
          }
        ],
        "qosClass": "BestEffort"
      }
    }
  ]
}`
)

type MockHTTPRoundTripper struct {
	http.RoundTripper
	mock.Mock
}

func (m *MockHTTPRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	args := m.Called()

	var resp *http.Response
	if args.Get(0) != nil {
		resp = args.Get(0).(*http.Response)
	}
	return resp, args.Error(1)
}

// The production backoff waits about 7.5s across its five steps, which would make every
// error-path test here that slow. These tests are about which requests are made, not how long
// the waits are, so they keep the step count and drop the waiting.
var fastTestBackoff = wait.Backoff{Steps: 5, Duration: time.Microsecond, Factor: 1.0}

func newTestKubeletHTTPSClient(responseCode int, responseBody string) *kubeletHTTPSClient {
	mockRoundTripper := new(MockHTTPRoundTripper)
	mockRoundTripper.On("RoundTrip", mock.Anything).Return(&http.Response{
		StatusCode: responseCode,
		Body:       io.NopCloser(strings.NewReader(responseBody)),
	}, nil)
	return &kubeletHTTPSClient{
		ctx:               context.Background(),
		httpRoundTripper:  mockRoundTripper,
		staticBearerToken: "authToken",
		listPodsURI:       "https://localhost:10250/pods",
		listPodsBackoff:   fastTestBackoff,
		listPodsTimeout:   testCallTimeout,
	}
}

// Generous, because these tests assert on which requests are made, not on timing. A zero value
// would be an already-expired deadline and no attempt would run at all.
const testCallTimeout = time.Minute

func TestListPods(t *testing.T) {
	client := newTestKubeletHTTPSClient(http.StatusOK, podJson)
	pods, err := client.ListPods()
	assert.NoError(t, err)
	assert.Equal(t, 2, len(pods))

	// ensure that we can extract the devices annotation
	expectedDeviceAnnotation := model.DeviceAnnotation{
		Devices: map[string][]string{
			"nvidia.com/gpu": {
				"GPU-455d8f70-2051-db6c-0430-ffc457bff834",
			},
		},
	}
	deviceAnnotation, ok := pods[0].GetAnnotations()[model.PodDeviceAnnotationName]
	assert.True(t, ok)
	var actualDeviceAnnotation model.DeviceAnnotation
	err = json.Unmarshal([]byte(deviceAnnotation), &actualDeviceAnnotation)
	assert.NoError(t, err)
	assert.Equal(t, expectedDeviceAnnotation, actualDeviceAnnotation)
}

func TestListPodsWithReadFileError(t *testing.T) {
	client := newTestKubeletHTTPSClient(http.StatusOK, podJson)
	client.staticBearerToken = ""
	_, err := client.ListPods()
	assert.Error(t, err)
}

// This used to set client.ctx = nil, which no longer reaches http.NewRequestWithContext:
// ListPods derives its deadline from that context first, and deriving from nil panics. An
// unparseable URI hits the same error path and is a state the client could actually be in.
func TestListPodsWithNewRequestError(t *testing.T) {
	client := newTestKubeletHTTPSClient(http.StatusOK, podJson)
	client.listPodsURI = "://not-a-url"
	_, err := client.ListPods()
	assert.Error(t, err)
}

func TestListPodsWithRoundTripError(t *testing.T) {
	client := newTestKubeletHTTPSClient(http.StatusOK, podJson)
	mockRoundTripper := new(MockHTTPRoundTripper)
	mockRoundTripper.On("RoundTrip", mock.Anything).Return((*http.Response)(nil),
		errors.New("network error"))
	client.httpRoundTripper = mockRoundTripper
	_, err := client.ListPods()
	assert.Error(t, err)
}

func TestListPodsWithInvalidResponseCode(t *testing.T) {
	client := newTestKubeletHTTPSClient(http.StatusInternalServerError, podJson)
	_, err := client.ListPods()
	assert.Error(t, err)
}

func TestListPodsWithRetry(t *testing.T) {
	mockRoundTripper := new(MockHTTPRoundTripper)
	mockRoundTripper.On("RoundTrip", mock.Anything).Return(&http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil).Once()
	mockRoundTripper.On("RoundTrip", mock.Anything).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(podJson)),
	}, nil).Once()
	client := &kubeletHTTPSClient{
		ctx:               context.Background(),
		httpRoundTripper:  mockRoundTripper,
		staticBearerToken: "authToken",
		listPodsURI:       "https://localhost:10250/pods",
		listPodsBackoff:   fastTestBackoff,
		listPodsTimeout:   testCallTimeout,
	}
	pods, err := client.ListPods()
	assert.NoError(t, err)
	assert.NotNil(t, pods)
}

// The point of widening the backoff is to outlast a credential rotation, which only works if a
// later attempt picks the new token up. Reading the token once per ListPods call would leave
// every attempt presenting the same expired one, so this asserts the second attempt sends the
// token that was written after the first attempt failed.
func TestListPods_TokenRotatedMidRetry_SecondAttemptSendsTheNewToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("stale-token"), 0o600))

	var sentTokens []string

	client := &kubeletHTTPSClient{
		ctx:             context.Background(),
		bearerTokenPath: tokenPath,
		listPodsURI:     "https://localhost:10250/pods",
		listPodsBackoff: fastTestBackoff,
		listPodsTimeout: testCallTimeout,
		httpRoundTripper: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			sentTokens = append(sentTokens, req.Header.Get("Authorization"))

			if len(sentTokens) == 1 {
				// Stand in for the kubelet rejecting the rotated-out credential, then the
				// kubelet refreshing the projected volume before the next attempt.
				require.NoError(t, os.WriteFile(tokenPath, []byte("fresh-token"), 0o600))

				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader("Unauthorized")),
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(podJson)),
			}, nil
		}),
	}

	pods, err := client.ListPods()
	require.NoError(t, err)
	assert.Len(t, pods, 2)

	require.Len(t, sentTokens, 2)
	assert.Equal(t, "Bearer stale-token", sentTokens[0])
	assert.Equal(t, "Bearer fresh-token", sentTokens[1],
		"the retry must re-read the token file, or widening the backoff cannot ride out a rotation")
}

// A kubelet that accepts the connection and then never answers is bounded by nothing else: the
// transport's timeouts cover only the header exchange, io.ReadAll has no deadline, and the
// retry sleeps ignore any context. Only the whole-call deadline cuts this short.
func TestListPods_HungKubelet_ReturnsWhenTheCallDeadlinePasses(t *testing.T) {
	const callTimeout = 150 * time.Millisecond

	client := &kubeletHTTPSClient{
		ctx:               context.Background(),
		staticBearerToken: "authToken",
		listPodsURI:       "https://localhost:10250/pods",
		listPodsBackoff:   fastTestBackoff,
		listPodsTimeout:   callTimeout,
		httpRoundTripper: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()

			return nil, req.Context().Err()
		}),
	}

	start := time.Now()
	_, err := client.ListPods()
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.GreaterOrEqual(t, elapsed, callTimeout)
	assert.Less(t, elapsed, 5*time.Second, "the deadline must abort the request, not wait it out")
}

// The invariant chain the production settings have to satisfy. retry.DefaultRetry waits about
// 40ms in total, shorter than any credential rotation, which is what #1767 traced the
// fleet-wide restarts to; but a long backoff is only safe if the whole call is bounded below
// the caller's poll period.
func TestNewKubeletHTTPSClient_BackoffOutlastsARotationAndStaysInsideThePollPeriod(t *testing.T) {
	client, err := NewKubeletHTTPSClient(context.Background())
	require.NoError(t, err)

	kubeletClient := client.(*kubeletHTTPSClient)
	backoff := kubeletClient.listPodsBackoff

	// wait.ExponentialBackoff sleeps *between* attempts and breaks before the last one, so
	// Steps attempts sleep Steps-1 times. Counting Steps sleeps overstates the total.
	require.Greater(t, backoff.Steps, 1)

	sleeps := time.Duration(0)
	step := backoff.Duration

	for range backoff.Steps - 1 {
		sleeps += step
		step = time.Duration(float64(step) * backoff.Factor)
	}

	assert.Greater(t, sleeps, 5*time.Second, "must outlast a credential rotation")
	assert.Less(t, sleeps, kubeletClient.listPodsTimeout,
		"the sleeps alone must not exhaust the call deadline, or no attempt gets to run")
	assert.Less(t, kubeletClient.listPodsTimeout, defaultPodDeviceMonitorPeriodForTest,
		"one call must finish inside the caller's poll period")
}

// The poll period lives in package main, so it is restated here rather than imported. Keep the
// two in step: the assertion above is only meaningful against the real cadence.
const defaultPodDeviceMonitorPeriodForTest = 30 * time.Second

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestListPodsWithUnmarshalError(t *testing.T) {
	client := newTestKubeletHTTPSClient(http.StatusOK, "invalid json response")
	_, err := client.ListPods()
	assert.Error(t, err)
}

func TestNewKubeletHTTPSClientHostResolution(t *testing.T) {
	tests := []struct {
		name     string
		hostEnv  string
		expected string
	}{
		{
			name:     "empty env defaults to localhost",
			hostEnv:  "",
			expected: "https://localhost:10250/pods",
		},
		{
			name:     "IPv4 host",
			hostEnv:  "192.168.1.100",
			expected: "https://192.168.1.100:10250/pods",
		},
		{
			name:     "IPv6 host is bracketed",
			hostEnv:  "fd00::1",
			expected: "https://[fd00::1]:10250/pods",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(kubeletHostEnvVar, tc.hostEnv)
			client, err := NewKubeletHTTPSClient(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, client.(*kubeletHTTPSClient).listPodsURI)
		})
	}
}
