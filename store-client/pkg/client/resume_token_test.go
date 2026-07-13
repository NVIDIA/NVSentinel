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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type mockResumeTokenDBClient struct {
	deleteCalls int
	tokenConfig TokenConfig
	deleteErr   error
}

func (m *mockResumeTokenDBClient) InsertMany(context.Context, []interface{}) (*InsertManyResult, error) {
	return nil, nil
}

func (m *mockResumeTokenDBClient) UpdateDocumentStatus(context.Context, string, string, interface{}) error {
	return nil
}

func (m *mockResumeTokenDBClient) UpdateDocumentStatusFields(context.Context, string, map[string]interface{}) error {
	return nil
}

func (m *mockResumeTokenDBClient) UpdateDocument(context.Context, interface{}, interface{}) (*UpdateResult, error) {
	return nil, nil
}

func (m *mockResumeTokenDBClient) UpdateManyDocuments(context.Context, interface{}, interface{}) (*UpdateResult, error) {
	return nil, nil
}

func (m *mockResumeTokenDBClient) UpsertDocument(context.Context, interface{}, interface{}) (*UpdateResult, error) {
	return nil, nil
}

func (m *mockResumeTokenDBClient) FindOne(context.Context, interface{}, *FindOneOptions) (SingleResult, error) {
	return nil, nil
}

func (m *mockResumeTokenDBClient) Find(context.Context, interface{}, *FindOptions) (Cursor, error) {
	return nil, nil
}

func (m *mockResumeTokenDBClient) CountDocuments(context.Context, interface{}, *CountOptions) (int64, error) {
	return 0, nil
}

func (m *mockResumeTokenDBClient) Aggregate(context.Context, interface{}) (Cursor, error) {
	return nil, nil
}

func (m *mockResumeTokenDBClient) Ping(context.Context) error {
	return nil
}

func (m *mockResumeTokenDBClient) NewChangeStreamWatcher(
	context.Context, TokenConfig, interface{},
) (ChangeStreamWatcher, error) {
	return nil, nil
}

func (m *mockResumeTokenDBClient) DeleteResumeToken(_ context.Context, tokenConfig TokenConfig) error {
	m.deleteCalls++
	m.tokenConfig = tokenConfig

	return m.deleteErr
}

func (m *mockResumeTokenDBClient) Close(context.Context) error {
	return nil
}

type mockChangeStreamWatcher struct{}

func (m *mockChangeStreamWatcher) Start(context.Context) {}

func (m *mockChangeStreamWatcher) Events() <-chan Event {
	return nil
}

func (m *mockChangeStreamWatcher) MarkProcessed(context.Context, []byte) error {
	return nil
}

func (m *mockChangeStreamWatcher) Close(context.Context) error {
	return nil
}

type mockMetricsChangeStreamWatcher struct {
	mockChangeStreamWatcher
	count           int64
	lastProcessedID string
}

func (m *mockMetricsChangeStreamWatcher) GetUnprocessedEventCount(
	_ context.Context,
	lastProcessedID string,
) (int64, error) {
	m.lastProcessedID = lastProcessedID

	return m.count, nil
}

type mockResumeControlStore struct {
	mode         string
	getErr       error
	setModeErr   error
	setCalls     int
	setClient    string
	setMode      string
	beginCalls   int
	beginErr     error
	lastClient   string
	cutoff       time.Time
	cutoffErr    error
	setCutoffErr error
	setCutoff    time.Time
}

func (m *mockResumeControlStore) GetMode(_ context.Context, clientName string) (string, error) {
	m.lastClient = clientName

	return m.mode, m.getErr
}

func (m *mockResumeControlStore) SetMode(_ context.Context, clientName, mode string) error {
	m.setCalls++
	m.setClient = clientName
	m.setMode = mode

	return m.setModeErr
}

func (m *mockResumeControlStore) GetColdStartCutoff(_ context.Context, _ string) (time.Time, error) {
	return m.cutoff, m.cutoffErr
}

func (m *mockResumeControlStore) SetColdStartCutoff(_ context.Context, _ string, cutoff time.Time) error {
	m.setCutoff = cutoff

	return m.setCutoffErr
}

func (m *mockResumeControlStore) BeginCreate(_ context.Context, clientName string, cutoff time.Time) error {
	m.beginCalls++
	m.setClient = clientName
	if m.beginErr != nil {
		return m.beginErr
	}

	m.mode = resumeControlModeCreating
	m.cutoff = cutoff

	return nil
}

func TestResumeControlChangeStreamWatcher_ForwardsUnprocessedEventCount(t *testing.T) {
	wrapped := &mockMetricsChangeStreamWatcher{count: 42}
	watcher := NewChangeStreamWatcherWithResumeControl(wrapped, ResumeControlDecision{})

	metricsWatcher, ok := watcher.(ChangeStreamMetrics)
	if !ok {
		t.Fatal("wrapped watcher does not implement ChangeStreamMetrics")
	}

	count, err := metricsWatcher.GetUnprocessedEventCount(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if count != 42 {
		t.Fatalf("count = %d, want 42", count)
	}

	if wrapped.lastProcessedID != "abc123" {
		t.Fatalf("lastProcessedID = %q, want abc123", wrapped.lastProcessedID)
	}
}

func TestResetResumeTokenOnStartWithStore_ResumeNoop(t *testing.T) {
	dbClient := &mockResumeTokenDBClient{}
	cutoff := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	store := &mockResumeControlStore{mode: ResumeControlModeResume, cutoff: cutoff}
	decision, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, TokenConfig{
		ClientName:      "node-drainer",
		TokenDatabase:   "HealthEventsDatabase",
		TokenCollection: "ResumeTokens",
	}, store)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dbClient.deleteCalls != 0 {
		t.Fatalf("DeleteResumeToken called %d times, want 0", dbClient.deleteCalls)
	}

	if decision.StartFresh {
		t.Fatal("StartFresh = true, want false")
	}

	if !decision.ColdStartCutoff.Equal(cutoff) {
		t.Fatalf("ColdStartCutoff = %v, want %v", decision.ColdStartCutoff, cutoff)
	}

	if store.setCalls != 0 {
		t.Fatalf("SetMode called %d times, want 0", store.setCalls)
	}
}

func TestResetResumeTokenOnStartIfConfigured_SkipsEventExporter(t *testing.T) {
	dbClient := &mockResumeTokenDBClient{}
	decision, err := ResetResumeTokenOnStartIfConfigured(context.Background(), dbClient, TokenConfig{
		ClientName:      "event-exporter",
		TokenDatabase:   "HealthEventsDatabase",
		TokenCollection: "ResumeTokens",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dbClient.deleteCalls != 0 {
		t.Fatalf("DeleteResumeToken called %d times, want 0", dbClient.deleteCalls)
	}

	if decision.StartFresh {
		t.Fatal("StartFresh = true, want false")
	}
}

func TestResetResumeTokenOnStartWithStore_CreateDeletesTokenAndResetsMode(t *testing.T) {
	tokenConfig := TokenConfig{
		ClientName:      "node-drainer",
		TokenDatabase:   "HealthEventsDatabase",
		TokenCollection: "ResumeTokens",
	}
	dbClient := &mockResumeTokenDBClient{}
	store := &mockResumeControlStore{mode: ResumeControlModeCreate}

	decision, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, tokenConfig, store)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dbClient.deleteCalls != 1 {
		t.Fatalf("DeleteResumeToken called %d times, want 1", dbClient.deleteCalls)
	}

	if dbClient.tokenConfig != tokenConfig {
		t.Fatalf("DeleteResumeToken got token config %+v, want %+v", dbClient.tokenConfig, tokenConfig)
	}

	if !decision.StartFresh {
		t.Fatal("StartFresh = false, want true")
	}

	if decision.ColdStartCutoff.IsZero() {
		t.Fatal("ColdStartCutoff is zero, want CREATE cutoff")
	}

	if !store.cutoff.Equal(decision.ColdStartCutoff) {
		t.Fatalf("BeginCreate cutoff got %v, want %v", store.cutoff, decision.ColdStartCutoff)
	}

	if store.beginCalls != 1 {
		t.Fatalf("BeginCreate called %d times, want 1", store.beginCalls)
	}

	if store.setCalls != 1 {
		t.Fatalf("SetMode called %d times, want 1", store.setCalls)
	}

	if store.setClient != tokenConfig.ClientName || store.setMode != ResumeControlModeResume {
		t.Fatalf("SetMode got client=%q mode=%q, want client=%q mode=%q",
			store.setClient, store.setMode, tokenConfig.ClientName, ResumeControlModeResume)
	}
}

func TestResetResumeTokenOnStartWithStore_CreateWithoutColdStartCutoffSupport(t *testing.T) {
	tokenConfig := TokenConfig{
		ClientName:      "fault-quarantine",
		TokenDatabase:   "HealthEventsDatabase",
		TokenCollection: "ResumeTokens",
	}
	dbClient := &mockResumeTokenDBClient{}
	store := &mockResumeControlStore{mode: ResumeControlModeCreate}

	decision, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, tokenConfig, store)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dbClient.deleteCalls != 1 {
		t.Fatalf("DeleteResumeToken called %d times, want 1", dbClient.deleteCalls)
	}

	if !decision.StartFresh {
		t.Fatal("StartFresh = false, want true")
	}

	if !decision.ColdStartCutoff.IsZero() {
		t.Fatalf("ColdStartCutoff = %v, want zero", decision.ColdStartCutoff)
	}

	if !store.setCutoff.IsZero() {
		t.Fatalf("SetColdStartCutoff got %v, want zero", store.setCutoff)
	}
}

func TestResetResumeTokenOnStartWithStore_ReadError(t *testing.T) {
	readErr := errors.New("read failed")
	dbClient := &mockResumeTokenDBClient{}
	store := &mockResumeControlStore{getErr: readErr}

	_, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, TokenConfig{
		ClientName: "node-drainer",
	}, store)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, readErr) {
		t.Fatalf("errors.Is(err, readErr) = false, err=%v", err)
	}

	if !strings.Contains(err.Error(), "failed to read change stream resume control") {
		t.Fatalf("error %q missing read context", err)
	}

	if dbClient.deleteCalls != 0 {
		t.Fatalf("DeleteResumeToken called %d times, want 0", dbClient.deleteCalls)
	}
}

func TestResetResumeTokenOnStartWithStore_DeleteError(t *testing.T) {
	deleteErr := errors.New("delete failed")
	dbClient := &mockResumeTokenDBClient{deleteErr: deleteErr}
	store := &mockResumeControlStore{mode: ResumeControlModeCreate}

	_, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, TokenConfig{
		ClientName: "node-drainer",
	}, store)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, deleteErr) {
		t.Fatalf("errors.Is(err, deleteErr) = false, err=%v", err)
	}

	if !strings.Contains(err.Error(), "failed to delete change stream resume token") {
		t.Fatalf("error %q missing delete context", err)
	}

	if dbClient.deleteCalls != 1 {
		t.Fatalf("DeleteResumeToken called %d times, want 1", dbClient.deleteCalls)
	}

	if store.setCalls != 0 {
		t.Fatalf("SetMode called %d times, want 0", store.setCalls)
	}
}

func TestResetResumeTokenOnStartWithStore_ResetModeError(t *testing.T) {
	setErr := errors.New("set failed")
	dbClient := &mockResumeTokenDBClient{}
	store := &mockResumeControlStore{mode: ResumeControlModeCreate, setModeErr: setErr}

	_, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, TokenConfig{
		ClientName: "node-drainer",
	}, store)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, setErr) {
		t.Fatalf("errors.Is(err, setErr) = false, err=%v", err)
	}

	if !strings.Contains(err.Error(), "failed to reset change stream resume control") {
		t.Fatalf("error %q missing reset context", err)
	}

	if dbClient.deleteCalls != 1 {
		t.Fatalf("DeleteResumeToken called %d times, want 1", dbClient.deleteCalls)
	}
}

func TestResetResumeTokenOnStartWithStore_BeginCreateError(t *testing.T) {
	setErr := errors.New("begin create failed")
	dbClient := &mockResumeTokenDBClient{}
	store := &mockResumeControlStore{mode: ResumeControlModeCreate, beginErr: setErr}

	_, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, TokenConfig{
		ClientName: "node-drainer",
	}, store)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, setErr) {
		t.Fatalf("errors.Is(err, setErr) = false, err=%v", err)
	}

	if !strings.Contains(err.Error(), "failed to begin resume-control CREATE") {
		t.Fatalf("error %q missing begin-create context", err)
	}

	if dbClient.deleteCalls != 0 {
		t.Fatalf("DeleteResumeToken called %d times, want 0", dbClient.deleteCalls)
	}
}

func TestResetResumeTokenOnStartWithStore_RetryReusesPersistedCutoff(t *testing.T) {
	deleteErr := errors.New("delete failed")
	dbClient := &mockResumeTokenDBClient{deleteErr: deleteErr}
	store := &mockResumeControlStore{mode: ResumeControlModeCreate}

	_, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, TokenConfig{
		ClientName: "node-drainer",
	}, store)
	if err == nil {
		t.Fatal("expected first attempt to fail")
	}

	persistedCutoff := store.cutoff
	if persistedCutoff.IsZero() {
		t.Fatal("persisted cutoff is zero")
	}
	eventAfterOriginalCutoff := persistedCutoff.Add(time.Second)

	dbClient.deleteErr = nil
	decision, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, TokenConfig{
		ClientName: "node-drainer",
	}, store)
	if err != nil {
		t.Fatalf("unexpected retry error: %v", err)
	}

	if !decision.StartFresh {
		t.Fatal("StartFresh = false, want true")
	}

	if !decision.ColdStartCutoff.Equal(persistedCutoff) {
		t.Fatalf("retry cutoff = %v, want persisted cutoff %v", decision.ColdStartCutoff, persistedCutoff)
	}

	if !eventAfterOriginalCutoff.After(decision.ColdStartCutoff) {
		t.Fatalf("event at %v would be skipped by retry cutoff %v", eventAfterOriginalCutoff, decision.ColdStartCutoff)
	}

	if store.beginCalls != 1 {
		t.Fatalf("BeginCreate called %d times, want 1", store.beginCalls)
	}
}

func TestResetResumeTokenOnStartWithStore_InvalidMode(t *testing.T) {
	dbClient := &mockResumeTokenDBClient{}
	store := &mockResumeControlStore{mode: "MAYBE"}

	_, err := resetResumeTokenOnStartWithStore(context.Background(), dbClient, TokenConfig{
		ClientName: "node-drainer",
	}, store)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "invalid change stream resume control mode") {
		t.Fatalf("error %q missing invalid mode context", err)
	}

	if dbClient.deleteCalls != 0 {
		t.Fatalf("DeleteResumeToken called %d times, want 0", dbClient.deleteCalls)
	}
}

func TestKubernetesResumeControlStore_GetModeCreatesMissingConfigMap(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	store := &kubernetesResumeControlStore{
		client:    clientset,
		name:      "resume-control",
		namespace: "nvsentinel",
	}

	mode, err := store.GetMode(ctx, "node-drainer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mode != ResumeControlModeResume {
		t.Fatalf("mode = %q, want %q", mode, ResumeControlModeResume)
	}

	cm, err := clientset.CoreV1().ConfigMaps("nvsentinel").Get(ctx, "resume-control", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ConfigMap to be created: %v", err)
	}

	if got := cm.Data["node-drainer"]; got != ResumeControlModeResume {
		t.Fatalf("node-drainer mode = %q, want %q", got, ResumeControlModeResume)
	}
}

func TestKubernetesResumeControlStore_GetModeMissingKeyWritesResume(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "resume-control", Namespace: "nvsentinel"},
		Data:       map[string]string{"event-exporter": ResumeControlModeCreate},
	})
	store := &kubernetesResumeControlStore{
		client:    clientset,
		name:      "resume-control",
		namespace: "nvsentinel",
	}

	mode, err := store.GetMode(ctx, "node-drainer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mode != ResumeControlModeResume {
		t.Fatalf("mode = %q, want %q", mode, ResumeControlModeResume)
	}

	cm, err := clientset.CoreV1().ConfigMaps("nvsentinel").Get(ctx, "resume-control", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ConfigMap to exist: %v", err)
	}

	if got := cm.Data["node-drainer"]; got != ResumeControlModeResume {
		t.Fatalf("node-drainer mode = %q, want %q", got, ResumeControlModeResume)
	}
}

func TestKubernetesResumeControlStore_SetModePreservesExistingKeys(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "resume-control", Namespace: "nvsentinel"},
		Data:       map[string]string{"event-exporter": ResumeControlModeCreate},
	})
	store := &kubernetesResumeControlStore{
		client:    clientset,
		name:      "resume-control",
		namespace: "nvsentinel",
	}

	if err := store.SetMode(ctx, "node-drainer", ResumeControlModeResume); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := clientset.CoreV1().ConfigMaps("nvsentinel").Get(ctx, "resume-control", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ConfigMap to exist: %v", err)
	}

	if got := cm.Data["event-exporter"]; got != ResumeControlModeCreate {
		t.Fatalf("event-exporter mode = %q, want %q", got, ResumeControlModeCreate)
	}

	if got := cm.Data["node-drainer"]; got != ResumeControlModeResume {
		t.Fatalf("node-drainer mode = %q, want %q", got, ResumeControlModeResume)
	}
}

func TestKubernetesResumeControlStore_ColdStartCutoffRoundTrip(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "resume-control", Namespace: "nvsentinel"},
		Data:       map[string]string{"node-drainer": ResumeControlModeResume},
	})
	store := &kubernetesResumeControlStore{
		client:    clientset,
		name:      "resume-control",
		namespace: "nvsentinel",
	}
	cutoff := time.Date(2026, 7, 10, 10, 0, 0, 123, time.UTC)

	if err := store.SetColdStartCutoff(ctx, "node-drainer", cutoff); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := store.GetColdStartCutoff(ctx, "node-drainer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !got.Equal(cutoff) {
		t.Fatalf("cutoff = %v, want %v", got, cutoff)
	}
}

func TestKubernetesResumeControlStore_BeginCreateSetsPhaseAndCutoff(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "resume-control", Namespace: "nvsentinel"},
		Data:       map[string]string{"node-drainer": ResumeControlModeCreate},
	})
	store := &kubernetesResumeControlStore{
		client:    clientset,
		name:      "resume-control",
		namespace: "nvsentinel",
	}
	cutoff := time.Date(2026, 7, 10, 10, 0, 0, 123, time.UTC)

	if err := store.BeginCreate(ctx, "node-drainer", cutoff); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := clientset.CoreV1().ConfigMaps("nvsentinel").Get(ctx, "resume-control", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ConfigMap to exist: %v", err)
	}

	if got := cm.Data["node-drainer"]; got != resumeControlModeCreating {
		t.Fatalf("node-drainer mode = %q, want %q", got, resumeControlModeCreating)
	}

	if got := cm.Data[coldStartCutoffKey("node-drainer")]; got != cutoff.Format(time.RFC3339Nano) {
		t.Fatalf("cutoff = %q, want %q", got, cutoff.Format(time.RFC3339Nano))
	}
}

func TestKubernetesResumeControlStore_BeginCreateOmitsZeroCutoff(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "resume-control", Namespace: "nvsentinel"},
		Data:       map[string]string{"fault-quarantine": ResumeControlModeCreate},
	})
	store := &kubernetesResumeControlStore{
		client:    clientset,
		name:      "resume-control",
		namespace: "nvsentinel",
	}

	if err := store.BeginCreate(ctx, "fault-quarantine", time.Time{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := clientset.CoreV1().ConfigMaps("nvsentinel").Get(ctx, "resume-control", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected ConfigMap to exist: %v", err)
	}

	if got := cm.Data["fault-quarantine"]; got != resumeControlModeCreating {
		t.Fatalf("fault-quarantine mode = %q, want %q", got, resumeControlModeCreating)
	}

	if _, ok := cm.Data[coldStartCutoffKey("fault-quarantine")]; ok {
		t.Fatal("fault-quarantine cutoff key was written for module without cold-start cutoff support")
	}
}
