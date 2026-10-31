// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"path/filepath"
	"testing"

	"github.com/dscsystems/go-iec61850/mms"
	"github.com/dscsystems/go-iec61850/model"
)

func TestTyped(t *testing.T) {
	cases := []struct {
		kind  mms.Type
		btype string
		raw   any
	}{
		{mms.TypeBoolean, "BOOLEAN", true},
		{mms.TypeInteger, "INT32", -3.0},
		{mms.TypeUnsigned, "INT32U", 3.0},
		{mms.TypeFloat32, "FLOAT32", 1.5},
		{mms.TypeFloat64, "FLOAT64", 1.5},
		{mms.TypeVisibleString, "VisString64", "x"},
		{mms.TypeMMSString, "Unicode255", "x"},
		{mms.TypeBitString, "Dbpos", 2.0},
	}
	for _, c := range cases {
		v, err := typed(&model.DataAttribute{Kind: c.kind, BType: c.btype}, c.raw)
		if err != nil {
			t.Errorf("%v: %v", c.kind, err)
			continue
		}
		if v.Type() != c.kind {
			t.Errorf("%v: built %v", c.kind, v.Type())
		}
	}
}

func TestTypedRejects(t *testing.T) {
	cases := []struct {
		kind  mms.Type
		btype string
		raw   any
	}{
		{mms.TypeBoolean, "BOOLEAN", 1.0},
		{mms.TypeInteger, "INT32", "1"},
		{mms.TypeUnsigned, "INT32U", -1.0},
		{mms.TypeFloat32, "FLOAT32", true},
		{mms.TypeVisibleString, "VisString64", 1.0},
		{mms.TypeBitString, "Quality", 2.0}, // only Dbpos has an enumeration
		{mms.TypeOctetString, "Octet64", "00"},
	}
	for _, c := range cases {
		if _, err := typed(&model.DataAttribute{Kind: c.kind, BType: c.btype}, c.raw); err == nil {
			t.Errorf("%v/%s accepted %v", c.kind, c.btype, c.raw)
		}
	}
}

func TestLoadValuesReadsTheFixture(t *testing.T) {
	m, err := loadValues(filepath.Join("..", "..", "..", "..", "fixtures", "iec61850", "values.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) == 0 {
		t.Fatal("no values")
	}
	if _, err := loadValues(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("absent file accepted")
	}
}
