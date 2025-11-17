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
	"log/slog"
)

// init sets up fallback functions for builder creation
// This is needed to avoid circular import dependencies
func init() {
	// Set up fallback functions that will be used if no factory is registered
	// These will be overridden when a specific provider (like MongoDB) is imported
	newMongoFilterBuilder = func() FilterBuilder {
		slog.Warn("No builder factory registered, using default MongoDB builders")
		// This is a minimal fallback - in practice, importing the mongodb package
		// will register the proper factory
		return &defaultFilterBuilder{}
	}

	newMongoUpdateBuilder = func() UpdateBuilder {
		slog.Warn("No builder factory registered, using default MongoDB builders")
		return &defaultUpdateBuilder{}
	}
}

// defaultFilterBuilder provides a minimal fallback implementation
type defaultFilterBuilder struct {
	filters map[string]interface{}
}

func (f *defaultFilterBuilder) Eq(field string, value interface{}) FilterBuilder {
	if f.filters == nil {
		f.filters = make(map[string]interface{})
	}

	f.filters[field] = value

	return f
}

func (f *defaultFilterBuilder) Ne(field string, value interface{}) FilterBuilder {
	if f.filters == nil {
		f.filters = make(map[string]interface{})
	}

	f.filters[field] = map[string]interface{}{"$ne": value}

	return f
}

func (f *defaultFilterBuilder) Gt(field string, value interface{}) FilterBuilder {
	if f.filters == nil {
		f.filters = make(map[string]interface{})
	}

	f.filters[field] = map[string]interface{}{"$gt": value}

	return f
}

func (f *defaultFilterBuilder) Gte(field string, value interface{}) FilterBuilder {
	if f.filters == nil {
		f.filters = make(map[string]interface{})
	}

	f.filters[field] = map[string]interface{}{"$gte": value}

	return f
}

func (f *defaultFilterBuilder) Lt(field string, value interface{}) FilterBuilder {
	if f.filters == nil {
		f.filters = make(map[string]interface{})
	}

	f.filters[field] = map[string]interface{}{"$lt": value}

	return f
}

func (f *defaultFilterBuilder) Lte(field string, value interface{}) FilterBuilder {
	if f.filters == nil {
		f.filters = make(map[string]interface{})
	}

	f.filters[field] = map[string]interface{}{"$lte": value}

	return f
}

func (f *defaultFilterBuilder) In(field string, values interface{}) FilterBuilder {
	if f.filters == nil {
		f.filters = make(map[string]interface{})
	}

	f.filters[field] = map[string]interface{}{"$in": values}

	return f
}

func (f *defaultFilterBuilder) And(filters ...interface{}) FilterBuilder {
	if f.filters == nil {
		f.filters = make(map[string]interface{})
	}

	f.filters["$and"] = filters

	return f
}

func (f *defaultFilterBuilder) Or(filters ...interface{}) FilterBuilder {
	if f.filters == nil {
		f.filters = make(map[string]interface{})
	}

	f.filters["$or"] = filters

	return f
}

func (f *defaultFilterBuilder) Build() interface{} {
	if f.filters == nil {
		return map[string]interface{}{}
	}

	return f.filters
}

// defaultUpdateBuilder provides a minimal fallback implementation
type defaultUpdateBuilder struct {
	updates map[string]interface{}
}

func (u *defaultUpdateBuilder) Set(field string, value interface{}) UpdateBuilder {
	if u.updates == nil {
		u.updates = make(map[string]interface{})
	}

	if u.updates["$set"] == nil {
		u.updates["$set"] = make(map[string]interface{})
	}

	u.updates["$set"].(map[string]interface{})[field] = value

	return u
}

func (u *defaultUpdateBuilder) Unset(field string) UpdateBuilder {
	if u.updates == nil {
		u.updates = make(map[string]interface{})
	}

	if u.updates["$unset"] == nil {
		u.updates["$unset"] = make(map[string]interface{})
	}

	u.updates["$unset"].(map[string]interface{})[field] = ""

	return u
}

func (u *defaultUpdateBuilder) Inc(field string, value interface{}) UpdateBuilder {
	if u.updates == nil {
		u.updates = make(map[string]interface{})
	}

	if u.updates["$inc"] == nil {
		u.updates["$inc"] = make(map[string]interface{})
	}

	u.updates["$inc"].(map[string]interface{})[field] = value

	return u
}

func (u *defaultUpdateBuilder) Build() interface{} {
	if u.updates == nil {
		return map[string]interface{}{}
	}

	return u.updates
}
