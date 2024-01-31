// Copyright 2020 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.
package main

import (
	pb "go.chromium.org/chromiumos/vm_tools/vm_rpc"
	"log"
	"sort"
	"strings"
)

// Features represents the features that have been enabled.
type Features struct {
	features map[pb.StartTerminaRequest_Feature]bool
}

// The docs for flag don't actually say what this is for, but presumably it's to
// conform to the Stringer interface.
func (v *Features) String() string {
	if v.features == nil {
		return ""
	}

	var features []string
	for feature := range pb.StartTerminaRequest_Feature_name {
		if v.features[pb.StartTerminaRequest_Feature(feature)] {
			features = append(features, pb.StartTerminaRequest_Feature_name[feature])
		}
	}
	sort.Strings(features)
	return strings.Join(features, ",")
}

// Set is called when parsing CLI arguments when a new -feature parameter is
// encountered.
func (v *Features) Set(s string) error {
	log.Printf("Setting flag: %s", s)
	feature, ok := pb.StartTerminaRequest_Feature_value[s]
	if !ok {
		log.Printf("Ignoring unknown feature: %s", s)
	}
	if v.features == nil {
		v.features = make(map[pb.StartTerminaRequest_Feature]bool)
	}
	v.features[pb.StartTerminaRequest_Feature(feature)] = true
	return nil
}

// IsUsedByTestsEnabled reports whether the USED_BY_TESTS feature flag is set.
func (v *Features) IsUsedByTestsEnabled() bool {
	return v.features != nil && v.features[pb.StartTerminaRequest_USED_BY_TESTS]
}
