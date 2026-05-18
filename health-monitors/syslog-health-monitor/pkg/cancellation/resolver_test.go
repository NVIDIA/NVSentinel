// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package cancellation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewResolver_Lookup(t *testing.T) {
	r := NewResolver(&CheckCancellations{
		Name:    "SysLogsXIDError",
		Enabled: true,
		Rules: []CancellationRule{
			{OnErrorCode: "162", CancelErrorCodes: []string{"163"}},
			{OnErrorCode: "31", CancelErrorCodes: []string{"13", "43"}},
		},
	})

	assert.Equal(t, []string{"163"}, r["162"])
	assert.Equal(t, []string{"13", "43"}, r["31"])
	assert.Nil(t, r["999"])
	assert.Len(t, r, 2)
}

func TestNewResolver_NilCheckIsEmpty(t *testing.T) {
	r := NewResolver(nil)
	assert.Nil(t, r)
	// Nil map lookup is the canonical "no rule" path used by callers.
	assert.Nil(t, r["162"])
	assert.Equal(t, 0, len(r))
}

func TestNewResolver_DisabledCheckIsEmpty(t *testing.T) {
	r := NewResolver(&CheckCancellations{
		Name:    "SysLogsXIDError",
		Enabled: false,
		Rules: []CancellationRule{
			{OnErrorCode: "162", CancelErrorCodes: []string{"163"}},
		},
	})

	assert.Nil(t, r)
	assert.Nil(t, r["162"])
}
