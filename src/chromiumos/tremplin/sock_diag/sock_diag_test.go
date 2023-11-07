// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package sock_diag

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestSockDiagMarshal(t *testing.T) {
	s := SockDiagReq{
		Family:   unix.AF_INET,
		Protocol: unix.IPPROTO_TCP,
		Ext:      0,
		States:   1 << TCP_LISTEN,
		SocketID: SockID{},
	}

	b, err := s.MarshalBinary()
	if err != nil {
		t.Fatalf("Failed to marshal SockDiagReq: %v", err)
	}
	if len(b) != 56 {
		t.Fatalf("Marshaled SockDiagReq is len %d, not expected 56", len(b))
	}
}

func TestDumpSockDiag(t *testing.T) {
	ports, err := GetListeningLocalhostPorts(0)

	if err != nil {
		t.Fatalf("Failed to get listening ports: %v", err)
	}

	t.Logf("Got %d listening ports", len(ports))
}
