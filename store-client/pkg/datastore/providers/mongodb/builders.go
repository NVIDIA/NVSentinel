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

package mongodb

import (
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"go.mongodb.org/mongo-driver/bson"
)

// mongoFilterBuilder provides MongoDB-specific filter building capabilities
type mongoFilterBuilder struct {
	filter bson.M
}

// mongoUpdateBuilder provides MongoDB-specific update building capabilities
type mongoUpdateBuilder struct {
	update bson.M
}

// Eq adds an equality filter
func (fb *mongoFilterBuilder) Eq(field string, value interface{}) client.FilterBuilder {
	fb.filter[field] = value
	return fb
}

// Ne adds a not-equal filter
func (fb *mongoFilterBuilder) Ne(field string, value interface{}) client.FilterBuilder {
	fb.filter[field] = bson.M{"$ne": value}
	return fb
}

// Gt adds a greater-than filter
func (fb *mongoFilterBuilder) Gt(field string, value interface{}) client.FilterBuilder {
	fb.filter[field] = bson.M{"$gt": value}
	return fb
}

// Gte adds a greater-than-or-equal filter
func (fb *mongoFilterBuilder) Gte(field string, value interface{}) client.FilterBuilder {
	fb.filter[field] = bson.M{"$gte": value}
	return fb
}

// Lt adds a less-than filter
func (fb *mongoFilterBuilder) Lt(field string, value interface{}) client.FilterBuilder {
	fb.filter[field] = bson.M{"$lt": value}
	return fb
}

// Lte adds a less-than-or-equal filter
func (fb *mongoFilterBuilder) Lte(field string, value interface{}) client.FilterBuilder {
	fb.filter[field] = bson.M{"$lte": value}
	return fb
}

// In adds an "in" filter for array membership
func (fb *mongoFilterBuilder) In(field string, values interface{}) client.FilterBuilder {
	fb.filter[field] = bson.M{"$in": values}
	return fb
}

// And combines multiple filters with logical AND
func (fb *mongoFilterBuilder) And(filters ...interface{}) client.FilterBuilder {
	fb.filter["$and"] = filters
	return fb
}

// Or combines multiple filters with logical OR
func (fb *mongoFilterBuilder) Or(filters ...interface{}) client.FilterBuilder {
	fb.filter["$or"] = filters
	return fb
}

// Build returns the final MongoDB filter document
func (fb *mongoFilterBuilder) Build() interface{} {
	return fb.filter
}

// Set adds a field set operation to the update
func (ub *mongoUpdateBuilder) Set(field string, value interface{}) client.UpdateBuilder {
	if ub.update["$set"] == nil {
		ub.update["$set"] = bson.M{}
	}

	ub.update["$set"].(bson.M)[field] = value

	return ub
}

// Unset adds a field unset operation to the update
func (ub *mongoUpdateBuilder) Unset(field string) client.UpdateBuilder {
	if ub.update["$unset"] == nil {
		ub.update["$unset"] = bson.M{}
	}

	ub.update["$unset"].(bson.M)[field] = ""

	return ub
}

// Inc adds a field increment operation to the update
func (ub *mongoUpdateBuilder) Inc(field string, value interface{}) client.UpdateBuilder {
	if ub.update["$inc"] == nil {
		ub.update["$inc"] = bson.M{}
	}

	ub.update["$inc"].(bson.M)[field] = value

	return ub
}

// Build returns the final MongoDB update document
func (ub *mongoUpdateBuilder) Build() interface{} {
	return ub.update
}

// mongoBuilderFactory implements the client.BuilderFactory interface for MongoDB
type mongoBuilderFactory struct{}

// NewFilterBuilder creates a new MongoDB filter builder
func (f *mongoBuilderFactory) NewFilterBuilder() client.FilterBuilder {
	return &mongoFilterBuilder{filter: bson.M{}}
}

// NewUpdateBuilder creates a new MongoDB update builder
func (f *mongoBuilderFactory) NewUpdateBuilder() client.UpdateBuilder {
	return &mongoUpdateBuilder{update: bson.M{}}
}

// NewMongoBuilderFactory creates a new MongoDB builder factory
func NewMongoBuilderFactory() client.BuilderFactory {
	return &mongoBuilderFactory{}
}
