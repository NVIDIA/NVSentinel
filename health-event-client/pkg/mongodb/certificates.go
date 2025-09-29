/*
Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mongodb

import (
	"fmt"
	"os"
)

// certificateManager implements CertificateManager interface
type certificateManager struct{}

// NewCertificateManager creates a new certificate manager
func NewCertificateManager() CertificateManager {
	return &certificateManager{}
}

// WriteCertificateFiles writes certificate files to disk
func (cm *certificateManager) WriteCertificateFiles(caCert, tlsCert, tlsKey []byte) error {
	var errs []error

	if err := os.WriteFile(CAFile, caCert, 0600); err != nil {
		errs = append(errs, fmt.Errorf("failed to write ca file: %w", err))
	}

	combinedCreds := make([]byte, 0, len(tlsCert)+len(tlsKey))
	combinedCreds = append(combinedCreds, tlsCert...)
	combinedCreds = append(combinedCreds, tlsKey...)

	if err := os.WriteFile(CredsFile, combinedCreds, 0600); err != nil {
		errs = append(errs, fmt.Errorf("failed to write creds file: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("certificate write errors: %v", errs)
	}

	return nil
}

// CleanupCertificateFiles removes temporary certificate files
func (cm *certificateManager) CleanupCertificateFiles() error {
	var errs []error

	if err := os.Remove(CAFile); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("failed to remove %s: %w", CAFile, err))
	}

	if err := os.Remove(CredsFile); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("failed to remove %s: %w", CredsFile, err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("certificate cleanup errors: %v", errs)
	}

	return nil
}
