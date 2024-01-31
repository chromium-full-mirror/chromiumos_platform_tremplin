// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"chromiumos/tremplin/sock_diag"
	pb "go.chromium.org/chromiumos/vm_tools/tremplin_proto"

	"github.com/elastic/go-libaudit"
	"github.com/elastic/go-libaudit/auparse"
	"github.com/elastic/go-libaudit/rule"
	"github.com/lxc/lxd/shared/api"
)

const errAuditRuleExists = "rule exists"

// UpdatePorts queries for listening ports in all running containers and sends
// those ports to the host.
func (s *tremplinServer) UpdatePorts() error {
	if s.lxd == nil {
		return nil
	}
	containers, err := s.lxd.GetInstances("container")
	if err != nil {
		return fmt.Errorf("failed to get container list: %v", err)
	}

	req := &pb.ListeningPortInfo{
		ContainerPorts: map[string]*pb.ListeningPortInfo_ContainerPortInfo{},
	}

	for _, c := range containers {
		state, _, err := s.lxd.GetInstanceState(c.Name)
		if err != nil {
			return fmt.Errorf("failed to get container state for %q: %v", c.Name, err)
		}

		// Only check running containers.
		if state.StatusCode != api.Running {
			continue
		}

		// Get the netns fd for the container's init process.
		nsPath := fmt.Sprintf("/proc/%d/ns/net", state.Pid)
		f, err := os.Open(nsPath)
		if err != nil {
			return fmt.Errorf("failed to open container netns path %s for %q: %v", nsPath, c.Name, err)
		}
		defer f.Close()

		ports, err := sock_diag.GetListeningLocalhostPorts(f.Fd())
		if err != nil {
			return fmt.Errorf("failed to get listening ports for %q: %v", c.Name, err)
		}

		// The ports need to be converted to uint32, since protobuf doesn't have
		// a uint16 type.
		uint32Ports := []uint32{}
		for _, v := range ports {
			uint32Ports = append(uint32Ports, uint32(v))
		}

		req.ContainerPorts[c.Name] = &pb.ListeningPortInfo_ContainerPortInfo{
			ListeningTcp4Ports: uint32Ports,
		}
	}

	_, err = s.listenerClient.UpdateListeningPorts(context.Background(), req)
	if err != nil {
		return fmt.Errorf("failed to get update listening ports: %v", err)
	}

	return nil
}

func (s *tremplinServer) startAuditListener() error {
	if s.auditClient == nil {
		var err error
		s.auditClient, err = libaudit.NewAuditClient(nil)
		if err != nil {
			return fmt.Errorf("failed to create audit client: %v", err)
		}
	} else {
		log.Print("Found an existing audit client so reusing. Did a previous launch fail?")
	}

	status, err := s.auditClient.GetStatus()
	if err != nil {
		return fmt.Errorf("failed to get audit status: %v", err)
	}

	if status.Enabled == 0 {
		log.Print("Enabling kernel audit subsystem")
		if err = s.auditClient.SetEnabled(true, libaudit.WaitForReply); err != nil {
			return fmt.Errorf("failed to enable auditing: %v", err)
		}
	}

	if err := s.auditClient.SetPID(libaudit.WaitForReply); err != nil {
		return fmt.Errorf("failed to set tremplin as audit daemon: %v", err)
	}

	if err := s.auditClient.SetFailure(libaudit.LogOnFailure, libaudit.WaitForReply); err != nil {
		return fmt.Errorf("failed to set audit to log on failure: %v", err)
	}

	auditArches, err := getAuditArches()
	if err != nil {
		return fmt.Errorf("failed to get audit arches: %v", err)
	}

	for _, arch := range auditArches {
		compiledRule, err := rule.Build(&rule.SyscallRule{
			Type:   rule.AppendSyscallRuleType,
			List:   "exit",
			Action: "always",
			Filters: []rule.FilterSpec{rule.FilterSpec{
				Type:       rule.ValueFilterType,
				LHS:        "arch",
				Comparator: "=",
				RHS:        arch,
			}},
			Syscalls: []string{"listen"},
			Keys:     nil,
		})
		if err != nil {
			return fmt.Errorf("failed to compile listen rule: %v", err)
		}

		if err := s.auditClient.AddRule(compiledRule); err != nil && err.Error() != errAuditRuleExists {
			return fmt.Errorf("failed to add listen rule: %v", err)
		}

	}

	go func() {
		// Listen for audit events forever.
		for {
			rawMsg, err := s.auditClient.Receive(false)
			if err != nil {
				log.Printf("Failed to receive audit message: %v", err)
				break
			}

			// Syscalls are accompanied with PROCTITLE and EOE. Ignore those events.
			if rawMsg.Type != auparse.AUDIT_SYSCALL {
				continue
			}

			msg, err := auparse.Parse(rawMsg.Type, string(rawMsg.Data))
			if err != nil {
				log.Printf("Failed to parse audit message: %v", err)
				break
			}

			msgMap, err := msg.Data()
			if err != nil {
				log.Printf("Failed to parse message to map: %v", err)
				break
			}

			// audit isn't namespaced, so just treat any successful listen as a
			// potential new listener.
			if msgMap["syscall"] != "listen" || msgMap["result"] != "success" {
				continue
			}

			if err := s.UpdatePorts(); err != nil {
				log.Printf("Failed to update localhost forwarding ports: %v", err)
			}
		}
	}()

	// Spawn another goroutine that periodically cleans up ports. Audit can be used
	// to detect listening sockets, but it's not as easy to find out when a socket
	// is no longer listening.
	go func() {
		for {
			time.Sleep(1 * time.Minute)
			if err := s.UpdatePorts(); err != nil {
				log.Printf("Failed to send periodic listening ports update: %v", err)
			}
		}
	}()

	return nil
}

// Gets the architectures for the audit subsystem based on the Go runtime arch.
func getAuditArches() ([]string, error) {
	// All VM-capable Chrome OS machines are 64-bit, but may have 32-bit
	// userspace for ARM.
	switch runtime.GOARCH {
	case "amd64":
		return []string{"x86_64", "i386"}, nil
	case "arm64", "arm":
		return []string{"aarch64", "arm"}, nil
	default:
		return nil, fmt.Errorf("unknown arch for audit: %s", runtime.GOARCH)
	}
}
