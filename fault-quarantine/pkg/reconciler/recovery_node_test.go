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

package reconciler

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
)

func TestHasExistingQuarantineReturnsRecoveryNodeLookupFailure(t *testing.T) {
	lookupErr := errors.New("API server unavailable")
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, lookupErr
	})
	r := &Reconciler{k8sClient: &informer.FaultQuarantineClient{Clientset: clientset}}

	annotations, quarantined, err := r.hasExistingQuarantine(
		coldstart.WithRecoveryContext(context.Background()), "node-a")

	require.ErrorIs(t, err, lookupErr)
	assert.Nil(t, annotations)
	assert.False(t, quarantined)
}
