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

package central

import (
	"crypto/tls"
	"fmt"
	"path/filepath"

	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

// newCertWatcher builds the hot-reloading certificate source for
// tls.crt/tls.key under certDir; it is how cert-manager rotation takes effect
// without a restart (transport security). The pair is loaded eagerly, so a
// missing or invalid certificate fails startup rather than the first
// handshake. Handshakes are served from the cached pair, which survives
// non-atomic rotations (including delete-then-write): the watcher re-reads on
// file events with a periodic poll as fallback and only replaces the cache
// once a complete pair parses.
func newCertWatcher(certDir string) (*certwatcher.CertWatcher, error) {
	cw, err := certwatcher.New(filepath.Join(certDir, "tls.crt"), filepath.Join(certDir, "tls.key"))
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS certificate from %s: %w", certDir, err)
	}

	return cw, nil
}

// tlsConfigFor builds the listener's TLS configuration around the watcher's
// reloading callback.
func tlsConfigFor(cw *certwatcher.CertWatcher) *tls.Config {
	return &tls.Config{
		GetCertificate: cw.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}
