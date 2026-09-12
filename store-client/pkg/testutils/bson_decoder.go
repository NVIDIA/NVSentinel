// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

package testutils

import (
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// NewBSONDecoder encodes a test document once and returns a repeatable decoder.
// Consumers can exercise MongoDB decoding without importing the driver outside
// store-client. In particular, one document can be decoded into both a complete
// event and a narrow recovery identity after malformed fields reject the event.
func NewBSONDecoder(document map[string]any) (func(any) error, error) {
	encoded, err := bson.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode BSON test document: %w", err)
	}

	return func(value any) error {
		if err := bson.Unmarshal(encoded, value); err != nil {
			return fmt.Errorf("decode BSON test document: %w", err)
		}

		return nil
	}, nil
}
