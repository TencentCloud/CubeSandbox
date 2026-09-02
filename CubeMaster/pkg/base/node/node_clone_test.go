// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package node

import (
	"reflect"
	"testing"
	"time"
)

// TestCloneCopiesEveryExportedField guards against the explicit-field Clone
// silently dropping newly added Node fields: it populates every exported
// field with a non-zero value, clones, and compares field by field. When this
// test fails after adding a field, extend Clone accordingly.
func TestCloneCopiesEveryExportedField(t *testing.T) {
	source := &Node{}
	populateNonZero(reflect.ValueOf(source).Elem())
	source.SetSchedulingDisabled(true)

	cloned := source.Clone()
	if cloned == nil {
		t.Fatal("Clone returned nil")
	}
	sourceValue := reflect.ValueOf(source).Elem()
	clonedValue := reflect.ValueOf(cloned).Elem()
	nodeType := sourceValue.Type()
	for i := 0; i < nodeType.NumField(); i++ {
		field := nodeType.Field(i)
		if !field.IsExported() {
			// schedulingDisabled is asserted below; labelsCache is
			// intentionally not copied (it is a derived cache).
			continue
		}
		want := sourceValue.Field(i).Interface()
		got := clonedValue.Field(i).Interface()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Clone dropped or altered field %s: got %v, want %v", field.Name, got, want)
		}
	}
	if !cloned.SchedulingDisabled() {
		t.Error("Clone dropped the schedulingDisabled cordon flag")
	}
	if cloned.labelsCache != nil && cloned.labelsCache == source.labelsCache {
		t.Error("Clone shared the derived labels cache with the source")
	}
}

// populateNonZero assigns a non-zero value to v, recursing into slices, maps,
// pointers and structs with exported fields.
func populateNonZero(value reflect.Value) {
	typ := value.Type()
	switch value.Kind() {
	case reflect.String:
		value.SetString("x")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(1)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(1.5)
	case reflect.Slice:
		slice := reflect.MakeSlice(typ, 1, 1)
		populateNonZero(slice.Index(0))
		value.Set(slice)
	case reflect.Map:
		m := reflect.MakeMapWithSize(typ, 1)
		key := reflect.New(typ.Key()).Elem()
		populateNonZero(key)
		elem := reflect.New(typ.Elem()).Elem()
		populateNonZero(elem)
		m.SetMapIndex(key, elem)
		value.Set(m)
	case reflect.Pointer:
		p := reflect.New(typ.Elem())
		populateNonZero(p.Elem())
		value.Set(p)
	case reflect.Struct:
		if typ == reflect.TypeOf(time.Time{}) {
			value.Set(reflect.ValueOf(time.Unix(1234, 0)))
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).IsExported() {
				populateNonZero(value.Field(i))
			}
		}
	}
}
