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

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/preflight/pkg/gang/coordinator"
	"github.com/nvidia/nvsentinel/preflight/pkg/webhook"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// caBundleLabel selects the copies when the bundle changes.
	caBundleLabel = "nvsentinel.nvidia.com/platform-connector-ca"
	// syncInterval is how often Start re-reads the CA file and retries the
	// namespaces whose copy could not be written.
	syncInterval = 10 * time.Second
)

// CABundleSync keeps a ConfigMap copy of the deployment platform connector CA
// bundle in every tenant namespace the webhook injects into. The injected
// checks mount that copy to verify the server. Reads go through the manager's
// API reader so no cluster wide ConfigMap informer is started; writes use the
// manager's client.
type CABundleSync struct {
	client client.Client
	reader client.Reader
	caFile string

	// last holds the bundle bytes the sync loop saw on its previous tick. It
	// is nil until the first successful read, so the first tick sweeps every
	// copy once, which fixes copies left behind by a rotation while the
	// controller was down.
	last []byte

	// mu guards pending.
	mu sync.Mutex
	// pending holds the namespaces whose last Ensure failed. The sync loop
	// retries them on every tick until the copy is written, so a webhook
	// failure does not depend on the namespace label to be repaired.
	pending map[string]struct{}
}

// NewCABundleSync builds the sync for the CA file at caFile.
func NewCABundleSync(c client.Client, reader client.Reader, caFile string) *CABundleSync {
	return &CABundleSync{
		client:  c,
		reader:  reader,
		caFile:  caFile,
		pending: map[string]struct{}{},
	}
}

// Ensure makes the ConfigMap copy in namespace exist and carry the current
// bundle. It creates the copy when missing and updates it when the bundle
// differs. An AlreadyExists on create means another caller won the race, so
// the copy is read again and compared like any existing one. A failure
// records the namespace as pending so Start retries it; a success clears it.
func (s *CABundleSync) Ensure(ctx context.Context, namespace string) error {
	err := s.ensure(ctx, namespace)
	if err != nil {
		s.markPending(namespace)

		return err
	}

	s.clearPending(namespace)

	return nil
}

func (s *CABundleSync) ensure(ctx context.Context, namespace string) error {
	ca, err := s.readBundle()
	if err != nil {
		return err
	}

	key := client.ObjectKey{Namespace: namespace, Name: webhook.HealthPublishCAConfigMapName}
	existing := &corev1.ConfigMap{}

	err = s.reader.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		err = s.client.Create(ctx, s.desired(namespace, ca))
		if err == nil {
			slog.Info("Created platform connector CA ConfigMap",
				"namespace", namespace, "configMap", webhook.HealthPublishCAConfigMapName)

			return nil
		}

		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create ConfigMap %s/%s: %w",
				namespace, webhook.HealthPublishCAConfigMapName, err)
		}

		err = s.reader.Get(ctx, key, existing)
	}

	if err != nil {
		return fmt.Errorf("failed to get ConfigMap %s/%s: %w", namespace, webhook.HealthPublishCAConfigMapName, err)
	}

	return s.updateIfDiffers(ctx, existing, ca)
}

// Start is the controller-runtime Runnable that follows CA rotation and
// repairs failed copies. Once at startup and then every syncInterval it
// re-reads the file, sweeps the labelled copies when the bytes changed, and
// retries every pending namespace. Failures are logged and retried on the
// next tick; the loop only stops with the context.
func (s *CABundleSync) Start(ctx context.Context) error {
	s.tick(ctx)

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick is one round of Start.
func (s *CABundleSync) tick(ctx context.Context) {
	s.refresh(ctx)
	s.retryPending(ctx)
}

// refresh re-reads the CA file and sweeps the copies when the bytes changed.
// last only advances when every copy was brought up to date, so a failed
// update is retried on the next tick.
func (s *CABundleSync) refresh(ctx context.Context) {
	ca, err := s.readBundle()
	if err != nil {
		slog.Error("Failed to read platform connector CA file", "path", s.caFile, "error", err)

		return
	}

	if bytes.Equal(ca, s.last) {
		return
	}

	slog.Info("Refreshing platform connector CA copies",
		"path", s.caFile, "configMap", webhook.HealthPublishCAConfigMapName, "startup", s.last == nil)

	if err := s.refreshCopies(ctx, ca); err != nil {
		slog.Error("Failed to refresh platform connector CA ConfigMaps",
			"configMap", webhook.HealthPublishCAConfigMapName, "error", err)

		return
	}

	s.last = ca
}

// retryPending runs Ensure again for every namespace whose last Ensure
// failed. Ensure itself drops a namespace from the set when it succeeds.
func (s *CABundleSync) retryPending(ctx context.Context) {
	for _, namespace := range s.pendingNamespaces() {
		if err := s.Ensure(ctx, namespace); err != nil {
			slog.Error("Platform connector CA ConfigMap still failing, will retry",
				"namespace", namespace, "configMap", webhook.HealthPublishCAConfigMapName, "error", err)

			continue
		}

		slog.Info("Repaired platform connector CA ConfigMap",
			"namespace", namespace, "configMap", webhook.HealthPublishCAConfigMapName)
	}
}

// refreshCopies lists the labelled copies in all namespaces and updates each
// whose data differs from ca.
func (s *CABundleSync) refreshCopies(ctx context.Context, ca []byte) error {
	var list corev1.ConfigMapList
	if err := s.reader.List(ctx, &list, client.MatchingLabels{caBundleLabel: "true"}); err != nil {
		return fmt.Errorf("failed to list platform connector CA ConfigMaps: %w", err)
	}

	var errs []error

	for i := range list.Items {
		if err := s.updateIfDiffers(ctx, &list.Items[i], ca); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// updateIfDiffers brings an existing ConfigMap of our name to the current
// bundle and labels. The labels matter as much as the data: refresh only
// sweeps labelled copies, so an unlabelled one would miss the next rotation.
func (s *CABundleSync) updateIfDiffers(ctx context.Context, existing *corev1.ConfigMap, ca []byte) error {
	if existing.Data[webhook.HealthPublishCAKey] == string(ca) && hasCABundleLabels(existing) {
		return nil
	}

	if existing.Data == nil {
		existing.Data = map[string]string{}
	}

	existing.Data[webhook.HealthPublishCAKey] = string(ca)

	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}

	for k, v := range caBundleLabels() {
		existing.Labels[k] = v
	}

	if err := s.client.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update ConfigMap %s/%s: %w", existing.Namespace, existing.Name, err)
	}

	slog.Info("Updated platform connector CA ConfigMap", "namespace", existing.Namespace, "configMap", existing.Name)

	return nil
}

func (s *CABundleSync) markPending(namespace string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pending[namespace] = struct{}{}
}

func (s *CABundleSync) clearPending(namespace string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.pending, namespace)
}

// pendingNamespaces returns a sorted snapshot of the pending set so the
// retry loop does not hold the lock while it talks to the API server.
func (s *CABundleSync) pendingNamespaces() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	namespaces := make([]string, 0, len(s.pending))
	for namespace := range s.pending {
		namespaces = append(namespaces, namespace)
	}

	sort.Strings(namespaces)

	return namespaces
}

func (s *CABundleSync) desired(namespace string, ca []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		Name:      webhook.HealthPublishCAConfigMapName,
		Namespace: namespace,
		Labels:    caBundleLabels(),
		Data: map[string]string{
			webhook.HealthPublishCAKey: string(ca),
		},
	}
}

func (s *CABundleSync) readBundle() ([]byte, error) {
	ca, err := os.ReadFile(s.caFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read platform connector CA file %s: %w", s.caFile, err)
	}

	if len(bytes.TrimSpace(ca)) == 0 {
		return nil, fmt.Errorf("platform connector CA file %s is empty", s.caFile)
	}

	return ca, nil
}

func hasCABundleLabels(cm *corev1.ConfigMap) bool {
	for k, v := range caBundleLabels() {
		if cm.Labels[k] != v {
			return false
		}
	}

	return true
}

func caBundleLabels() map[string]string {
	return map[string]string{
		coordinator.ConfigMapLabelManagedBy: "preflight",
		caBundleLabel:                       "true",
	}
}
