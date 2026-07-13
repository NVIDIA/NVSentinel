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

package client

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
)

const (
	ResumeControlConfigMapName = "resume-control"
	ResumeControlModeResume    = "RESUME"
	ResumeControlModeCreate    = "CREATE"
	resumeControlModeCreating  = "CREATING"
)

// ResumeControlDecision describes the startup behavior selected by resume-control.
type ResumeControlDecision struct {
	StartFresh      bool
	ColdStartCutoff time.Time
}

// ChangeStreamWatcherWithResumeControl exposes the resume-control decision attached to a watcher.
type ChangeStreamWatcherWithResumeControl interface {
	ChangeStreamWatcher
	ResumeControlDecision() ResumeControlDecision
}

type resumeControlChangeStreamWatcher struct {
	ChangeStreamWatcher
	decision ResumeControlDecision
}

// NewChangeStreamWatcherWithResumeControl attaches a resume-control decision to a watcher.
func NewChangeStreamWatcherWithResumeControl(
	watcher ChangeStreamWatcher,
	decision ResumeControlDecision,
) ChangeStreamWatcher {
	return &resumeControlChangeStreamWatcher{
		ChangeStreamWatcher: watcher,
		decision:            decision,
	}
}

func (w *resumeControlChangeStreamWatcher) ResumeControlDecision() ResumeControlDecision {
	return w.decision
}

// GetUnprocessedEventCount forwards backlog metrics to the wrapped watcher when supported.
func (w *resumeControlChangeStreamWatcher) GetUnprocessedEventCount(
	ctx context.Context,
	lastProcessedID string,
) (int64, error) {
	metricsWatcher, ok := w.ChangeStreamWatcher.(ChangeStreamMetrics)
	if !ok {
		return 0, fmt.Errorf("wrapped change stream watcher does not support metrics")
	}

	return metricsWatcher.GetUnprocessedEventCount(ctx, lastProcessedID)
}

type resumeControlStore interface {
	GetMode(ctx context.Context, clientName string) (string, error)
	SetMode(ctx context.Context, clientName, mode string) error
	GetColdStartCutoff(ctx context.Context, clientName string) (time.Time, error)
	SetColdStartCutoff(ctx context.Context, clientName string, cutoff time.Time) error
	BeginCreate(ctx context.Context, clientName string, cutoff time.Time) error
}

// ResetResumeTokenOnStartIfConfigured deletes a component's change stream
// resume token when the shared resume-control ConfigMap requests CREATE.
func ResetResumeTokenOnStartIfConfigured(
	ctx context.Context,
	dbClient DatabaseClient,
	tokenConfig TokenConfig,
) (ResumeControlDecision, error) {
	if tokenConfig.ClientName == "event-exporter" {
		return ResumeControlDecision{}, nil
	}

	store, err := newKubernetesResumeControlStore()
	if err != nil {
		return ResumeControlDecision{}, fmt.Errorf("failed to initialize change stream resume control: %w", err)
	}

	return resetResumeTokenOnStartWithStore(ctx, dbClient, tokenConfig, store)
}

func resetResumeTokenOnStartWithStore(
	ctx context.Context,
	dbClient DatabaseClient,
	tokenConfig TokenConfig,
	store resumeControlStore,
) (ResumeControlDecision, error) {
	mode, cutoff, err := readResumeControl(ctx, tokenConfig.ClientName, store)
	if err != nil {
		return ResumeControlDecision{}, fmt.Errorf("failed to read resume-control state: %w", err)
	}

	if mode == "" || mode == ResumeControlModeResume {
		return ResumeControlDecision{ColdStartCutoff: cutoff}, nil
	}

	cutoff, err = prepareCreateResumeControl(ctx, tokenConfig.ClientName, mode, cutoff, store)
	if err != nil {
		return ResumeControlDecision{}, fmt.Errorf("failed to prepare resume-control CREATE: %w", err)
	}

	return deleteResumeTokenAndResume(ctx, dbClient, tokenConfig, store, cutoff)
}

func readResumeControl(
	ctx context.Context,
	clientName string,
	store resumeControlStore,
) (string, time.Time, error) {
	mode, err := store.GetMode(ctx, clientName)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to read change stream resume control for %s: %w", clientName, err)
	}

	cutoff, err := readColdStartCutoff(ctx, clientName, store)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to read resume-control cold-start cutoff: %w", err)
	}

	return strings.ToUpper(strings.TrimSpace(mode)), cutoff, nil
}

func readColdStartCutoff(ctx context.Context, clientName string, store resumeControlStore) (time.Time, error) {
	cutoff, err := store.GetColdStartCutoff(ctx, clientName)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to read cold-start cutoff for %s: %w", clientName, err)
	}

	return cutoff, nil
}

func prepareCreateResumeControl(
	ctx context.Context,
	clientName string,
	mode string,
	cutoff time.Time,
	store resumeControlStore,
) (time.Time, error) {
	if mode != ResumeControlModeCreate && mode != resumeControlModeCreating {
		return time.Time{}, fmt.Errorf("invalid change stream resume control mode %q for %s", mode, clientName)
	}

	if mode == ResumeControlModeCreate {
		if supportsColdStartCutoff(clientName) {
			cutoff = time.Now().UTC()
		}

		if err := store.BeginCreate(ctx, clientName, cutoff); err != nil {
			return time.Time{}, fmt.Errorf("failed to begin resume-control CREATE for %s: %w", clientName, err)
		}
	} else if supportsColdStartCutoff(clientName) && cutoff.IsZero() {
		return time.Time{}, fmt.Errorf("missing cold-start cutoff for in-progress resume-control CREATE for %s", clientName)
	}

	return cutoff, nil
}

func deleteResumeTokenAndResume(
	ctx context.Context,
	dbClient DatabaseClient,
	tokenConfig TokenConfig,
	store resumeControlStore,
	cutoff time.Time,
) (ResumeControlDecision, error) {
	slog.InfoContext(ctx, "Deleting change stream resume token on startup",
		"clientName", tokenConfig.ClientName,
		"tokenDatabase", tokenConfig.TokenDatabase,
		"tokenCollection", tokenConfig.TokenCollection)

	if err := dbClient.DeleteResumeToken(ctx, tokenConfig); err != nil {
		return ResumeControlDecision{}, fmt.Errorf("failed to delete change stream resume token: %w", err)
	}

	if err := store.SetMode(ctx, tokenConfig.ClientName, ResumeControlModeResume); err != nil {
		return ResumeControlDecision{}, fmt.Errorf("failed to reset change stream resume control for %s to %s: %w",
			tokenConfig.ClientName, ResumeControlModeResume, err)
	}

	return ResumeControlDecision{StartFresh: true, ColdStartCutoff: cutoff}, nil
}

type kubernetesResumeControlStore struct {
	client    kubernetes.Interface
	name      string
	namespace string
}

func newKubernetesResumeControlStore() (*kubernetesResumeControlStore, error) {
	namespace, err := resumeControlNamespace()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve resume-control namespace: %w", err)
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load in-cluster Kubernetes config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	return &kubernetesResumeControlStore{
		client:    clientset,
		name:      ResumeControlConfigMapName,
		namespace: namespace,
	}, nil
}

func resumeControlNamespace() (string, error) {
	if namespace := os.Getenv("POD_NAMESPACE"); namespace != "" {
		return namespace, nil
	}

	namespaceBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "", fmt.Errorf("failed to determine namespace for change stream resume control: %w", err)
	}

	namespace := strings.TrimSpace(string(namespaceBytes))
	if namespace == "" {
		return "", fmt.Errorf("empty namespace for change stream resume control")
	}

	return namespace, nil
}

func (s *kubernetesResumeControlStore) GetMode(ctx context.Context, clientName string) (string, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if err := s.SetMode(ctx, clientName, ResumeControlModeResume); err != nil {
			return "", fmt.Errorf("failed to create default resume control config map: %w", err)
		}

		return ResumeControlModeResume, nil
	}

	if err != nil {
		return "", fmt.Errorf("failed to get resume control config map %s in namespace %s: %w",
			s.name, s.namespace, err)
	}

	if cm.Data == nil {
		if err := s.SetMode(ctx, clientName, ResumeControlModeResume); err != nil {
			return "", fmt.Errorf("failed to set default resume control mode for %s: %w", clientName, err)
		}

		return ResumeControlModeResume, nil
	}

	mode, ok := cm.Data[clientName]
	if !ok || strings.TrimSpace(mode) == "" {
		if err := s.SetMode(ctx, clientName, ResumeControlModeResume); err != nil {
			return "", fmt.Errorf("failed to set default resume control mode for %s: %w", clientName, err)
		}

		return ResumeControlModeResume, nil
	}

	return mode, nil
}

func (s *kubernetesResumeControlStore) SetMode(ctx context.Context, clientName, mode string) error {
	return s.setValue(ctx, clientName, mode)
}

func (s *kubernetesResumeControlStore) GetColdStartCutoff(
	ctx context.Context,
	clientName string,
) (time.Time, error) {
	key := coldStartCutoffKey(clientName)

	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return time.Time{}, nil
	}

	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get resume control config map %s in namespace %s: %w",
			s.name, s.namespace, err)
	}

	if cm.Data == nil || strings.TrimSpace(cm.Data[key]) == "" {
		return time.Time{}, nil
	}

	cutoff, err := time.Parse(time.RFC3339Nano, cm.Data[key])
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cold-start cutoff %q for %s: %w", cm.Data[key], clientName, err)
	}

	return cutoff, nil
}

func (s *kubernetesResumeControlStore) SetColdStartCutoff(
	ctx context.Context,
	clientName string,
	cutoff time.Time,
) error {
	return s.setValue(ctx, coldStartCutoffKey(clientName), cutoff.UTC().Format(time.RFC3339Nano))
}

func (s *kubernetesResumeControlStore) BeginCreate(ctx context.Context, clientName string, cutoff time.Time) error {
	values := map[string]string{
		clientName: resumeControlModeCreating,
	}
	if !cutoff.IsZero() {
		values[coldStartCutoffKey(clientName)] = cutoff.UTC().Format(time.RFC3339Nano)
	}

	return s.setValues(ctx, values)
}

func coldStartCutoffKey(clientName string) string {
	return clientName + ".coldStartAfter"
}

func supportsColdStartCutoff(clientName string) bool {
	switch clientName {
	case "node-drainer", "fault-remediation":
		return true
	default:
		return false
	}
}

func (s *kubernetesResumeControlStore) setValue(ctx context.Context, key, value string) error {
	return s.setValues(ctx, map[string]string{key: value})
}

func (s *kubernetesResumeControlStore) setValues(ctx context.Context, values map[string]string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace},
				Data:       map[string]string{},
			}
			for key, value := range values {
				cm.Data[key] = value
			}

			_, createErr := s.client.CoreV1().ConfigMaps(s.namespace).Create(ctx, cm, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(createErr) {
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, s.name, createErr)
			}

			return createErr
		}

		if err != nil {
			return err
		}

		if cm.Data == nil {
			cm.Data = map[string]string{}
		}

		for key, value := range values {
			cm.Data[key] = value
		}

		_, err = s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})

		return err
	})
	if err != nil {
		return fmt.Errorf("failed to update resume control config map %s in namespace %s: %w",
			s.name, s.namespace, err)
	}

	return nil
}
