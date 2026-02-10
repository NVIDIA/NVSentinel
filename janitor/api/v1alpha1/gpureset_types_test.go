// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha1

import (
	"context"
	"crypto/rand"
	"math/big"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// RandString generates a random string of a given length.
func RandString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		num, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		b[i] = letters[num.Int64()]
	}
	return string(b)
}

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

// The tests below are designed to validate the OpenAPI schema markers
// (`+kubebuilder:validation`) on the GPUReset CRD.

var _ = Describe("GPUReset Validation", func() {

	Context("When creating a GPUReset resource", func() {
		const (
			TestNodeName      = "test-node-1"
			GPUResetNamespace = "default"
		)
		var (
			gpuResetName string
			gpuReset     *GPUReset
		)

		BeforeEach(func() {
			gpuResetName = "test-gpureset-" + RandString(5)
			gpuReset = &GPUReset{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gpuResetName,
					Namespace: GPUResetNamespace,
				},
				Spec: GPUResetSpec{
					NodeName: TestNodeName,
				},
			}
		})

		AfterEach(func() {
			err := k8sClient.Get(context.Background(), types.NamespacedName{Name: gpuResetName, Namespace: GPUResetNamespace}, gpuReset)
			if err == nil {
				Expect(k8sClient.Delete(context.Background(), gpuReset)).To(Succeed())
			}
		})

		Context("with invalid GPUSelector UUIDs", func() {
			DescribeTable("it should be rejected",
				func(uuids []string) {
					gpuReset.Spec.Selector = &GPUSelector{UUIDs: uuids}

					err := k8sClient.Create(context.Background(), gpuReset)
					Expect(err).To(HaveOccurred())
					Expect(errors.IsInvalid(err)).To(BeTrue(), "error should be a validation error")
					Expect(err.Error()).To(ContainSubstring("spec.selector.uuids"))
				},
				Entry("when UUID is missing the 'GPU-' prefix", []string{"d2a3f4b5-c6d7-e8f9-a0b1-c2d3e4f5a6b7"}),
				Entry("when UUID has incorrect formatting", []string{"GPU-d2a3f4b5c6d7-e8f9-a0b1-c2d3e4f5a6b7"}),
				Entry("when UUID contains invalid characters", []string{"GPU-d2a3f4b5-c6d7-e8f9-a0b1-c2d3e4f5a6bX"}),
				Entry("when one of several UUIDs is invalid", []string{"GPU-d2a3f4b5-c6d7-e8f9-a0b1-c2d3e4f5a6b7", "invalid-format"}),
			)
		})

		Context("with invalid GPUSelector PCIBusIDs", func() {
			DescribeTable("it should be rejected",
				func(pciBusIDs []string) {
					gpuReset.Spec.Selector = &GPUSelector{PCIBusIDs: pciBusIDs}

					err := k8sClient.Create(context.Background(), gpuReset)
					Expect(err).To(HaveOccurred())
					Expect(errors.IsInvalid(err)).To(BeTrue(), "error should be a validation error")
					Expect(err.Error()).To(ContainSubstring("spec.selector.pciBusIDs"))
				},
				Entry("when PCIBusID has incorrect separators", []string{"0000:01-00.0"}),
				Entry("when PCIBusID has incorrect segment length", []string{"0000:01:000.0"}),
				Entry("when PCIBusID contains invalid characters", []string{"0000:01:00.G"}),
			)
		})

		Context("with a valid selector", func() {
			It("should be accepted", func() {
				gpuReset.Spec.Selector = &GPUSelector{
					UUIDs:     []string{"GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"},
					PCIBusIDs: []string{"0000:0a:00.0"},
				}
				Expect(k8sClient.Create(context.Background(), gpuReset)).To(Succeed())
			})
		})
	})
})
