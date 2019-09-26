// Copyright 2019 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	pb "chromiumos/vm_tools/tremplin_proto"
	"context"
	"errors"
	"fmt"
	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared/api"
	"google.golang.org/grpc"
	"testing"
	"time"
)

type lxdStub struct {
	lxd.ContainerServer
	operation lxd.Operation
	execError error
}

type operationStub struct {
	lxd.Operation
	apiOperation api.Operation
	waitTime     time.Duration
	waitError    error
}

type listenerStub struct {
	pb.TremplinListenerClient
	validator func(pb.UpgradeContainerProgress_Status)
}

func (s lxdStub) ExecContainer(containerName string, exec api.ContainerExecPost, args *lxd.ContainerExecArgs) (lxd.Operation, error) {
	return s.operation, s.execError
}

func (s operationStub) Wait() (err error) {
	time.Sleep(s.waitTime)
	return s.waitError
}

func (s operationStub) Get() api.Operation {
	return s.apiOperation
}

func (s listenerStub) UpgradeContainerStatus(ctx context.Context, in *pb.UpgradeContainerProgress, opts ...grpc.CallOption) (*pb.EmptyMessage, error) {
	if s.validator != nil {
		s.validator(in.Status)
	}
	return &pb.EmptyMessage{}, nil
}

func makeStubs(returnCode float64, waitTime time.Duration, validator func(pb.UpgradeContainerProgress_Status)) (*tremplinServer, *lxdStub, *operationStub) {
	metadata := map[string]interface{}{
		"return": returnCode,
	}
	apiOp := api.Operation{
		Metadata: metadata,
	}
	op := &operationStub{
		apiOperation: apiOp,
		waitTime:     waitTime,
		waitError:    nil,
	}
	listener := listenerStub{
		validator: validator,
	}

	lxd := &lxdStub{operation: op}

	server := &tremplinServer{
		lxd:                         lxd,
		upgradeStatus:               *NewTransactionMap(),
		listenerClient:              listener,
		upgradeClientUpdateInterval: 5 * time.Millisecond,
	}

	return server, lxd, op
}

func TestStartUpgradeContainerLimitOneInProgressPerContainer(t *testing.T) {
	// Our upgrade sleeps for a very long time in the background, we don't wait for or need it to finish so it's fine.
	// This needs to be long enough that the second Start runs before the first finishes.
	server, _, _ := makeStubs(0.0, 1*time.Minute, nil)

	status, _ := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, got status %s", status)
	}

	status, _ = server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_ALREADY_RUNNING {
		t.Fatalf("Failed to correctly fail when an upgrade is already running, got status %s", status)
	}

	status, _ = server.startUpgradeContainer("test2", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to allow multiple upgrades on separate containers, got status %s", status)
	}
}

// If the upgrade fails we should be able to try upgrading again.
func TestStartUpgradeContainerCanRetryAfterFailure(t *testing.T) {
	done := make(chan bool)
	callback := func(status pb.UpgradeContainerProgress_Status) {
		if status != pb.UpgradeContainerProgress_IN_PROGRESS {
			done <- true
		}
	}

	server, lxd, operation := makeStubs(0.0, 0*time.Millisecond, callback)

	// Immediate failure.
	lxd.execError = errors.New("I'm a test error :)")
	status, _ := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_FAILED {
		t.Fatal("StartUpgrade didn't fail when it should've failed")
	}

	// Eventual failure.
	lxd.execError = nil
	operation.apiOperation.Metadata["return"] = 1.0
	status, _ = server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start on retry after immediate failure, got status %s", status)
	}

	// Wait until the above completes.
	<-done
	status, _ = server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start on retry after eventual failure, got status %s", status)
	}
}

func TestUpgradeContainerVersionValidation(t *testing.T) {
	server, _, _ := makeStubs(127.0, 0*time.Millisecond, nil)

	tables := []struct {
		from     pb.UpgradeContainerRequest_Version
		to       pb.UpgradeContainerRequest_Version
		expected pb.UpgradeContainerResponse_Status
	}{
		{pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER, pb.UpgradeContainerResponse_STARTED},
		{pb.UpgradeContainerRequest_DEBIAN_BUSTER, pb.UpgradeContainerRequest_DEBIAN_BUSTER, pb.UpgradeContainerResponse_NOT_SUPPORTED},
		{pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerResponse_NOT_SUPPORTED},
		{pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_UNKNOWN, pb.UpgradeContainerResponse_NOT_SUPPORTED},
		{pb.UpgradeContainerRequest_UNKNOWN, pb.UpgradeContainerRequest_DEBIAN_BUSTER, pb.UpgradeContainerResponse_NOT_SUPPORTED},
	}

	for _, table := range tables {
		status, _ := server.startUpgradeContainer(fmt.Sprintf("%d->%d", table.from, table.to), table.from, table.to)
		if status != table.expected {
			t.Errorf("Status of (%s -> %s) was incorrect, got: %s, want: %s.", table.from, table.to, status, table.expected)
		}
	}
}

func TestUpgradeContainerSendsInProgressMessages(t *testing.T) {
	statusChannel := make(chan pb.UpgradeContainerProgress_Status)
	callback := func(status pb.UpgradeContainerProgress_Status) {
		statusChannel <- status
	}

	// Our upgrade sleeps for a very long time in the background, we don't wait for or need it to finish so it's fine.
	// This needs to be long enough that we see an in-progress message get sent.
	server, _, _ := makeStubs(0.0, 1*time.Minute, callback)

	status, _ := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)

	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, got %s", status)
	}
	if s := <-statusChannel; s != pb.UpgradeContainerProgress_IN_PROGRESS {
		t.Fatalf("Didn't get in-progress message, got %s", s)
	}
}

func TestUpgradeContainerSendsSuccessOnSuccessfulEnd(t *testing.T) {
	statusChannel := make(chan pb.UpgradeContainerProgress_Status)
	callback := func(status pb.UpgradeContainerProgress_Status) {
		if status != pb.UpgradeContainerProgress_IN_PROGRESS {
			statusChannel <- status
		}
	}

	server, _, _ := makeStubs(0.0, 0*time.Millisecond, callback)

	status, _ := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)

	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, git %s", status)
	}
	if s := <-statusChannel; s != pb.UpgradeContainerProgress_SUCCEEDED {
		t.Fatalf("Didn't get success message, got %s", s)
	}
}

func TestUpgradeContainerSendsFailureOnUnsuccessfulEnd(t *testing.T) {
	statusChannel := make(chan pb.UpgradeContainerProgress_Status)
	callback := func(status pb.UpgradeContainerProgress_Status) {
		if status != pb.UpgradeContainerProgress_IN_PROGRESS {
			statusChannel <- status
		}
	}

	server, _, _ := makeStubs(127.0, 0*time.Millisecond, callback)

	status, _ := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)

	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, got %s", status)
	}
	if s := <-statusChannel; s != pb.UpgradeContainerProgress_FAILED {
		t.Fatalf("Didn't get failure message, got %s", s)
	}
}

func TestUpgradeContainerSendsFailureOnLxdError(t *testing.T) {
	statusChannel := make(chan pb.UpgradeContainerProgress_Status)
	callback := func(status pb.UpgradeContainerProgress_Status) {
		if status != pb.UpgradeContainerProgress_IN_PROGRESS {
			statusChannel <- status
		}
	}

	server, _, op := makeStubs(0.0, 0*time.Millisecond, callback)
	op.waitError = errors.New("I'm a test error :)")

	status, _ := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerRequest_DEBIAN_BUSTER)

	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, got %s", status)
	}
	if s := <-statusChannel; s != pb.UpgradeContainerProgress_FAILED {
		t.Fatalf("Didn't get failure message, got %s", s)
	}
}
