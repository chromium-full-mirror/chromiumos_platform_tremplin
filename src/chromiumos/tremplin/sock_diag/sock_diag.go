// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package sock_diag

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/mdlayher/netlink"

	"golang.org/x/sys/unix"
)

// Netlink uses native byte ordering. Go does not provide a way to get the native byte ordering.
var NativeEndian = binary.LittleEndian

const (
	TCP_LISTEN          = 10
	SOCK_DIAG_BY_FAMILY = 20
)

// SockID is inet_diag_sockid from sock_diag(7).
type SockID struct {
	SrcPort   uint16
	DestPort  uint16
	SrcAddr   [16]uint8
	DestAddr  [16]uint8
	Interface uint32
	Cookie    [2]uint32
}

// SockDiagReq is inet_diag_req_v2 from sock_diag(7).
type SockDiagReq struct {
	Family   uint8
	Protocol uint8
	Ext      uint8
	States   uint32
	SocketID SockID
}

// SockDiagReq is inet_diag_msg from sock_diag(7).
type SockDiagMsg struct {
	Family  uint8
	State   uint8
	Timer   uint8
	Retrans uint8

	SocketID SockID

	Expires uint32
	RQueue  uint32
	WQueue  uint32
	UID     uint32
	Inode   uint32
}

// MarshalBinary marshals a SockDiagReq into a byte slice.
func (s SockDiagReq) MarshalBinary() ([]byte, error) {
	b := &bytes.Buffer{}

	fields := []interface{}{s.Family, s.Protocol, s.Ext, uint8(0), s.States}
	for _, v := range fields {
		if err := binary.Write(b, NativeEndian, v); err != nil {
			return nil, err
		}
	}

	if err := s.SocketID.marshal(b); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func (s SockID) marshal(b *bytes.Buffer) error {
	bigFields := []interface{}{s.SrcPort, s.DestPort}
	for _, v := range bigFields {
		if err := binary.Write(b, binary.BigEndian, v); err != nil {
			return err
		}
	}

	// SrcAddr and DestAddr are treated as byte arrays and so are already big endian.
	nativeFields := []interface{}{s.SrcAddr, s.DestAddr, s.Interface, s.Cookie}
	for _, v := range nativeFields {
		if err := binary.Write(b, NativeEndian, v); err != nil {
			return err
		}
	}

	return nil
}

func (s *SockID) unmarshal(r io.Reader) error {
	bigFields := []interface{}{&s.SrcPort, &s.DestPort}
	for _, v := range bigFields {
		if err := binary.Read(r, binary.BigEndian, v); err != nil {
			return err
		}
	}

	// SrcAddr and DestAddr are treated as byte arrays and so are already big endian.
	nativeFields := []interface{}{&s.SrcAddr, &s.DestAddr, &s.Interface, &s.Cookie}
	for _, v := range nativeFields {
		if err := binary.Read(r, NativeEndian, v); err != nil {
			return err
		}
	}

	return nil
}

// UnmarshalBinary unmarshals a SockDiagMsg from a byte slice.
func (s *SockDiagMsg) UnmarshalBinary(data []byte) error {
	r := bytes.NewReader(data)

	fields := []interface{}{&s.Family, &s.State, &s.Timer, &s.Retrans}
	for _, v := range fields {
		if err := binary.Read(r, NativeEndian, v); err != nil {
			return err
		}
	}

	if err := s.SocketID.unmarshal(r); err != nil {
		return err
	}

	fields = []interface{}{&s.Expires, &s.RQueue, &s.WQueue, &s.UID, &s.Inode}
	for _, v := range fields {
		if err := binary.Read(r, NativeEndian, v); err != nil {
			return err
		}
	}

	return nil
}

// GetListeningPorts returns a list of TCP ports that are listening on localhost
// in the netns fd provided.
func GetListeningLocalhostPorts(nsid uintptr) ([]uint16, error) {
	config := netlink.Config{
		NetNS: int(nsid),
	}

	c, err := netlink.Dial(unix.NETLINK_SOCK_DIAG, &config)
	if err != nil {
		return nil, fmt.Errorf("failed to dial netlink: %v", err)
	}
	defer c.Close()

	ports := map[uint16]struct{}{}
	dumpPorts := func(s SockDiagReq) error {
		b, err := s.MarshalBinary()
		if err != nil {
			return fmt.Errorf("failed to marshal request: %v", err)
		}

		req := netlink.Message{
			Header: netlink.Header{
				Flags: netlink.Request | netlink.Dump,
				Type:  SOCK_DIAG_BY_FAMILY,
			},
			Data: b,
		}

		msgs, err := c.Execute(req)
		if err != nil {
			return fmt.Errorf("failed to execute request: %v", err)
		}

		for _, msg := range msgs {
			var s SockDiagMsg
			if err := (&s).UnmarshalBinary(msg.Data); err != nil {
				return fmt.Errorf("failed to unmarshal SockDiagMsg: %v", err)
			}

			var ip net.IP
			if s.Family == unix.AF_INET {
				ip = net.IP(s.SocketID.SrcAddr[:4])
			} else {
				ip = net.IP(s.SocketID.SrcAddr[:])
			}
			if !ip.IsUnspecified() && !ip.IsLoopback() {
				continue
			}
			ports[s.SocketID.SrcPort] = struct{}{}
		}

		return nil
	}

	if err := dumpPorts(SockDiagReq{
		Family:   unix.AF_INET,
		Protocol: unix.IPPROTO_TCP,
		Ext:      0,
		States:   1 << TCP_LISTEN,
		SocketID: SockID{},
	}); err != nil {
		return nil, err
	}

	if err := dumpPorts(SockDiagReq{
		Family:   unix.AF_INET6,
		Protocol: unix.IPPROTO_TCP,
		Ext:      0,
		States:   1 << TCP_LISTEN,
		SocketID: SockID{},
	}); err != nil {
		return nil, err
	}

	// Collapse set of ports into a slice.
	p := []uint16{}
	for port := range ports {
		p = append(p, port)
	}

	return p, nil
}
