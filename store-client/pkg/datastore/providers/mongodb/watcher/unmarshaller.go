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

package watcher

import (
	"fmt"
	"reflect"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
)

// UnmarshalFullDocumentFromEvent unmarshals the fullDocument from event directly into result.
func UnmarshalFullDocumentFromEvent[T any](event Event, result *T) error {
	bsonBytes, err := marshalFullDocument(event)
	if err != nil {
		return err
	}

	if err := bson.Unmarshal(bsonBytes, result); err != nil {
		return fmt.Errorf("error unmarshaling BSON for event %+v into type %T: %w", event, result, err)
	}

	return nil
}

// CreateBsonTaggedStructType creates a new struct type with BSON tags based on JSON tags
func CreateBsonTaggedStructType(typ reflect.Type) reflect.Type {
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	var fields []reflect.StructField

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)

		// Skip unexported fields
		if field.PkgPath != "" {
			continue
		}

		jsonTag := field.Tag.Get("json")
		if jsonTag != "" {
			bsonTag := fmt.Sprintf(`bson:"%s"`, strings.ToLower(jsonTag))
			field.Tag = reflect.StructTag(fmt.Sprintf(`%s %s`, bsonTag, field.Tag))
		}

		// Recursively handle nested structs
		if field.Type.Kind() == reflect.Struct {
			field.Type = CreateBsonTaggedStructType(field.Type)
		} else if field.Type.Kind() == reflect.Ptr && field.Type.Elem().Kind() == reflect.Struct {
			elemType := field.Type.Elem()
			field.Type = reflect.PointerTo(CreateBsonTaggedStructType(elemType))
		}

		fields = append(fields, field)
	}

	return reflect.StructOf(fields)
}

// CopyStructFields copies values from one struct to another based on matching field names
func CopyStructFields(dst, src reflect.Value) {
	dstType := dst.Type()

	for i := 0; i < dst.NumField(); i++ {
		dstField := dst.Field(i)
		dstFieldType := dstType.Field(i)

		srcField := src.FieldByName(dstFieldType.Name)
		if !srcField.IsValid() || !dstField.CanSet() {
			continue
		}

		switch {
		case dstField.Kind() == reflect.Ptr && srcField.Kind() == reflect.Ptr:
			copyPtrField(dstField, srcField)
		case dstField.Kind() == reflect.Struct && srcField.Kind() == reflect.Struct:
			CopyStructFields(dstField, srcField)
		case dstField.Kind() == srcField.Kind():
			dstField.Set(srcField)
		}
	}
}

func copyPtrField(dstField, srcField reflect.Value) {
	if srcField.IsNil() {
		dstField.Set(reflect.Zero(dstField.Type()))

		return
	}

	dstField.Set(reflect.New(dstField.Type().Elem()))

	if dstField.Type().Elem().Kind() == reflect.Struct && srcField.Type().Elem().Kind() == reflect.Struct {
		CopyStructFields(dstField.Elem(), srcField.Elem())
	} else {
		dstField.Elem().Set(srcField.Elem())
	}
}

// UnmarshalFullDocumentToJsonTaggedStructFromEvent unmarshals the MongoDB fullDocument from
// event into result using the caller-provided bsonTaggedType (created via CreateBsonTaggedStructType)
// to bridge JSON-tagged Go structs with BSON-encoded MongoDB documents.
func UnmarshalFullDocumentToJsonTaggedStructFromEvent[T any](event Event,
	bsonTaggedType reflect.Type, result *T) error {
	bsonBytes, err := marshalFullDocument(event)
	if err != nil {
		return err
	}

	bsonTaggedResult := reflect.New(bsonTaggedType).Interface()

	if err := bson.Unmarshal(bsonBytes, bsonTaggedResult); err != nil {
		return fmt.Errorf("error unmarshaling BSON for event %+v into type %T: %w", event, result, err)
	}

	CopyStructFields(reflect.ValueOf(result).Elem(), reflect.ValueOf(bsonTaggedResult).Elem())

	return nil
}

func marshalFullDocument(event Event) ([]byte, error) {
	fullDocument, ok := event["fullDocument"]
	if !ok {
		return nil, fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	var document bson.M

	switch v := fullDocument.(type) {
	case bson.M:
		document = v
	case map[string]interface{}:
		document = bson.M(v)
	default:
		return nil, fmt.Errorf("unsupported fullDocument type %T: %+v", fullDocument, fullDocument)
	}

	bsonBytes, err := bson.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("error marshaling BSON for event %+v: %w", document, err)
	}

	return bsonBytes, nil
}
