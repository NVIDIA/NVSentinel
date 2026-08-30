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

package client

// permanentError marks a deterministic event failure that replay cannot fix.
type permanentError struct {
	cause error
}

func (e *permanentError) Error() string { return e.cause.Error() }
func (e *permanentError) Unwrap() error { return e.cause }
func (e *permanentError) permanent()    {}

// PermanentError marks an event-processing error as deterministic. The event
// processor checkpoints such events so one poison record cannot block later
// work indefinitely.
func PermanentError(err error) error {
	if err == nil || IsPermanentError(err) {
		return err
	}

	return &permanentError{cause: err}
}

// IsPermanentError reports whether the complete error represents a permanent
// failure. Joined errors are permanent only when every constituent is marked.
func IsPermanentError(err error) bool {
	if err == nil {
		return false
	}

	if _, ok := err.(interface{ permanent() }); ok {
		return true
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}

		for _, cause := range causes {
			if !IsPermanentError(cause) {
				return false
			}
		}

		return true
	}

	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsPermanentError(wrapped.Unwrap())
	}

	return false
}
