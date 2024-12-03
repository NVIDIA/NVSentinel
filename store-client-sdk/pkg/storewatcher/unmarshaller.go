package storewatcher

import (
	"fmt"
	"reflect"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
)

// UnmarshalEvent takes in an event and a pointer to a variable of any type, and unmarshals the event into the variable
func UnmarshalFullDocumentFromEvent[T any](event map[string]interface{}, result *T) error {
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	bsonBytes, err := bson.Marshal(document)
	if err != nil {
		return fmt.Errorf("error marshaling BSON for event %+v: %w", document, err)
	}

	if err := bson.Unmarshal(bsonBytes, result); err != nil {
		return fmt.Errorf("error unmarshaling BSON for event %+v into type %T: %w", document, result, err)
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
		if !srcField.IsValid() {
			continue
		}

		//nolint //reason: ignoring complex nested blocks
		if dstField.CanSet() {
			if dstField.Kind() == reflect.Ptr && srcField.Kind() == reflect.Ptr {
				if srcField.IsNil() {
					dstField.Set(reflect.Zero(dstField.Type()))
				} else {
					dstField.Set(reflect.New(dstField.Type().Elem()))
					CopyStructFields(dstField.Elem(), srcField.Elem())
				}
			} else if dstField.Kind() == reflect.Struct && srcField.Kind() == reflect.Struct {
				// Recursively copy nested structs
				CopyStructFields(dstField, srcField)
			} else if dstField.Kind() == srcField.Kind() {
				dstField.Set(srcField)
			}
		}
	}
}

// Unmarshalls from the mongodb fullDocument to Json tagged struct by internally converting
// json tags for fields to bson tags
func UnmarshalFullDocumentToJsonTaggedStructFromEvent[T any](event map[string]interface{},
	bsonTaggedType reflect.Type, result *T) error {
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	bsonBytes, err := bson.Marshal(document)
	if err != nil {
		return fmt.Errorf("error marshaling BSON for event %+v: %w", document, err)
	}

	bsonTaggedResult := reflect.New(bsonTaggedType).Interface()

	if err := bson.Unmarshal(bsonBytes, bsonTaggedResult); err != nil {
		return fmt.Errorf("error unmarshaling BSON for event %+v into type %T: %w", document, result, err)
	}

	// Copy the values from the bson tagged result to the original result
	CopyStructFields(reflect.ValueOf(result).Elem(), reflect.ValueOf(bsonTaggedResult).Elem())

	return nil
}
