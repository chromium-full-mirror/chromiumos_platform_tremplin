// Copyright 2020 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	pb "go.chromium.org/chromiumos/vm_tools/vm_rpc"
	"flag"
	"testing"
)

func TestParseNoArguments(t *testing.T) {
	var features Features
	fs := flag.NewFlagSet("test", flag.PanicOnError)
	fs.Var(&features, "feature", "features to enable")
	fs.Parse([]string{})

	if len(features.features) != 0 {
		t.Fatalf("Expected no features to be set, got: %v", len(features.features))
	}
}

func TestParseMultipleArguments(t *testing.T) {
	var features Features
	fs := flag.NewFlagSet("test", flag.PanicOnError)
	fs.Var(&features, "feature", "features to enable")
	fs.Parse([]string{"-feature", "UNKNOWN", "-feature", "USED_BY_TESTS"})

	if !features.features[pb.StartTerminaRequest_UNKNOWN] {
		t.Fatal("Expected UNKNOWN feature to be set, but it wasn't")
	}
	if !features.IsUsedByTestsEnabled() {
		t.Fatal("Expected USED_BY_TESTS feature to be set, but it wasn't")
	}
}

func TestString(t *testing.T) {
	var features Features
	fs := flag.NewFlagSet("test", flag.PanicOnError)
	fs.Var(&features, "feature", "features to enable")
	fs.Parse([]string{"-feature", "UNKNOWN", "-feature", "USED_BY_TESTS"})

	expected := "UNKNOWN,USED_BY_TESTS"
	got := features.String()

	if got != expected {
		t.Fatalf("Expected: \"%s\", got: \"%s\"", expected, got)
	}
}

func TestUnknownFlagsAllowed(t *testing.T) {
	// Termina ahd Chrome OS versions aren't locked together, so we need to
	// ignore any unrecognised flags for compatibility.
	var features Features
	fs := flag.NewFlagSet("test", flag.PanicOnError)
	fs.Var(&features, "feature", "features to enable")
	fs.Parse([]string{"-feature", "NOT_A_REAL_FLAG"})

	expected := "UNKNOWN" // proto2 maps unrecognised enums to the default.
	got := features.String()

	if features.String() != expected {
		t.Fatalf("Expected: \"%s\" got: \"%s\"", expected, got)
	}
}
