// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"testing"

	"github.com/dscsystems/go-iec61850/model"
)

func TestReasonString(t *testing.T) {
	cases := map[model.ReasonCode]string{
		model.ReasonDataChange:    "data-change",
		model.ReasonQualityChange: "quality-change",
		model.ReasonDataUpdate:    "data-update",
		model.ReasonIntegrity:     "integrity",
		model.ReasonGI:            "gi",
		0:                         "unknown",
		model.ReasonDataChange | model.ReasonQualityChange: "unknown", // the C adapter names single reasons only
	}
	for rc, want := range cases {
		if got := reasonString(rc); got != want {
			t.Errorf("%d: got %s, want %s", rc, got, want)
		}
	}
}
