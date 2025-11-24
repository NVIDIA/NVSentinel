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

package datastore

import (
	"time"
)

// DataStoreProvider defines the supported datastore types
type DataStoreProvider string

const (
	ProviderMongoDB    DataStoreProvider = "mongodb"
	ProviderPostgreSQL DataStoreProvider = "postgresql"
)

// Event represents a database-agnostic document or event as a map.
// This is the canonical type for representing database documents throughout the system,
// whether they come from change streams, queries, or inserts.
type Event map[string]interface{}

// DataStoreConfig holds configuration for any datastore provider
type DataStoreConfig struct {
	Provider   DataStoreProvider `json:"provider" yaml:"provider"`
	Connection ConnectionConfig  `json:"connection" yaml:"connection"`
	Options    map[string]string `json:"options,omitempty" yaml:"options,omitempty"`
}

// ConnectionConfig holds generic connection parameters
type ConnectionConfig struct {
	Host        string            `json:"host" yaml:"host"`
	Port        int               `json:"port" yaml:"port"`
	Database    string            `json:"database" yaml:"database"`
	Username    string            `json:"username,omitempty" yaml:"username,omitempty"`
	Password    string            `json:"password,omitempty" yaml:"password,omitempty"`
	SSLMode     string            `json:"sslmode,omitempty" yaml:"sslmode,omitempty"`
	SSLCert     string            `json:"sslcert,omitempty" yaml:"sslcert,omitempty"`
	SSLKey      string            `json:"sslkey,omitempty" yaml:"sslkey,omitempty"`
	SSLRootCert string            `json:"sslrootcert,omitempty" yaml:"sslrootcert,omitempty"`
	TLSConfig   *TLSConfig        `json:"tls,omitempty" yaml:"tls,omitempty"`
	ExtraParams map[string]string `json:"extraParams,omitempty" yaml:"extraParams,omitempty"`
}

// TLSConfig holds TLS certificate configuration
type TLSConfig struct {
	CertPath string `json:"certPath" yaml:"certPath"`
	KeyPath  string `json:"keyPath" yaml:"keyPath"`
	CAPath   string `json:"caPath" yaml:"caPath"`
}

// Status represents operation status
type Status string

const (
	StatusNotStarted   Status = "NotStarted"
	StatusInProgress   Status = "InProgress"
	StatusFailed       Status = "Failed"
	StatusSucceeded    Status = "Succeeded"
	AlreadyDrained     Status = "AlreadyDrained"
	UnQuarantined      Status = "UnQuarantined"
	Quarantined        Status = "Quarantined"
	AlreadyQuarantined Status = "AlreadyQuarantined"
)

// OperationStatus represents status of an operation
type OperationStatus struct {
	Status   Status                 `json:"status"`
	Message  string                 `json:"message,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// HealthEventStatus represents status of a health event
type HealthEventStatus struct {
	NodeQuarantined          *Status         `json:"nodequarantined"`
	UserPodsEvictionStatus   OperationStatus `json:"userpodsevictionstatus"`
	FaultRemediated          *bool           `json:"faultremediated"`
	LastRemediationTimestamp *time.Time      `json:"lastremediationtimestamp,omitempty"`
}

// HealthEventWithStatus wraps a health event with status information
type HealthEventWithStatus struct {
	CreatedAt         time.Time         `json:"createdAt"`
	HealthEvent       interface{}       `json:"healthevent,omitempty"`
	HealthEventStatus HealthEventStatus `json:"healtheventstatus"`
	RawEvent          Event             // Raw event from change stream (for extracting MongoDB _id etc)
}

// EventWithToken wraps a change stream event with its corresponding resume token
type EventWithToken struct {
	Event       Event
	ResumeToken []byte // Provider-agnostic binary token for resume position
}

// Database-agnostic pipeline types for query operations across different datastore providers

// Element represents a key-value pair in a database-agnostic way
type Element struct {
	Key   string      `json:"key"`
	Value interface{} `json:"value"`
}

// Document represents a collection of key-value pairs (like a MongoDB document or PostgreSQL JSON object)
type Document []Element

// Array represents an array of values in a database-agnostic way
type Array []interface{}

// Pipeline represents a sequence of operations that can be applied across different datastore providers
type Pipeline []Document

// Convenience constructors for building pipeline operations

// E creates an Element with the given key and value
func E(key string, value interface{}) Element {
	return Element{Key: key, Value: value}
}

// D creates a Document from a list of Elements
func D(elements ...Element) Document {
	return Document(elements)
}

// A creates an Array from a list of values
func A(values ...interface{}) Array {
	return Array(values)
}

// ToMap converts a Document to a map[string]interface{} for compatibility
func (d Document) ToMap() map[string]interface{} {
	result := make(map[string]interface{}, len(d))
	for _, elem := range d {
		result[elem.Key] = elem.Value
	}

	return result
}

// FromMap creates a Document from a map[string]interface{}
func FromMap(m map[string]interface{}) Document {
	doc := make(Document, 0, len(m))
	for k, v := range m {
		doc = append(doc, Element{Key: k, Value: v})
	}

	return doc
}

// ToPipeline converts a slice of Document operations into a Pipeline
func ToPipeline(docs ...Document) Pipeline {
	return Pipeline(docs)
}
