// Copyright 2019 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	pb "chromiumos/vm_tools/tremplin_proto"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"strings"
	"testing"
	"time"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared/api"
	"google.golang.org/grpc"
)

type lxdStub struct {
	lxd.ContainerServer
	operation *operationStub
	execError error
	osRelease string
	lastExec  *api.ContainerExecPost
}

type operationStub struct {
	lxd.Operation
	apiOperation api.Operation
	waitTime     time.Duration
	waitError    error
	out          io.WriteCloser
}

type listenerStub struct {
	pb.TremplinListenerClient
	validator func(*pb.UpgradeContainerProgress)
}

type containerFileServerStub struct {
	lxd *lxdStub
}

func (s *lxdStub) ExecContainer(containerName string, exec api.ContainerExecPost, args *lxd.ContainerExecArgs) (lxd.Operation, error) {
	s.operation.out = args.Stdout
	s.lastExec = &exec
	args.Stdout.Write([]byte("In-progress message\n"))
	return s.operation, s.execError
}

func (s *lxdStub) UpdateContainerState(containerName string, statePut api.ContainerStatePut, etag string) (lxd.Operation, error) {
	return s.operation, s.execError
}

func (s *lxdStub) GetContainerFile(containerName, path string) (io.ReadCloser, *lxd.ContainerFileResponse, error) {
	reader := ioutil.NopCloser(strings.NewReader(s.osRelease))
	resp := lxd.ContainerFileResponse{}
	return reader, &resp, nil
}

func (s *containerFileServerStub) GetContainerFile(path string) (io.ReadCloser, *lxd.ContainerFileResponse, error) {
	return s.lxd.GetContainerFile("unused", path)
}

func (s *containerFileServerStub) CreateContainerFile(path string, args lxd.ContainerFileArgs) (err error) {
	log.Fatal("Not implemented")
	return nil
}

func (s *containerFileServerStub) DeleteContainerFile(path string) (err error) {
	log.Fatal("Not implemented")
	return nil
}

func (s operationStub) Wait() (err error) {
	time.Sleep(s.waitTime)
	s.out.Write([]byte("Last in-progress message\nDone message\n"))
	return s.waitError
}

func (s operationStub) Get() api.Operation {
	return s.apiOperation
}

func (s listenerStub) UpgradeContainerStatus(ctx context.Context, in *pb.UpgradeContainerProgress, opts ...grpc.CallOption) (*pb.EmptyMessage, error) {
	if s.validator != nil {
		s.validator(in)
	}
	return &pb.EmptyMessage{}, nil
}

func (s listenerStub) ContainerShutdown(ctx context.Context, in *pb.ContainerShutdownInfo, opts ...grpc.CallOption) (*pb.EmptyMessage, error) {
	return &pb.EmptyMessage{}, nil
}

func makeOsRelease(codename string) string {
	return fmt.Sprintf("ID=debian\nVERSION_CODENAME=%s", codename)
}

func makeStubs(returnCode float64, waitTime time.Duration, validator func(*pb.UpgradeContainerProgress)) (*tremplinServer, *lxdStub, *operationStub) {
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

	lxd := &lxdStub{
		operation: op,
		osRelease: makeOsRelease("stretch"),
	}

	cfs := &containerFileServerStub{}
	OverrideContainerFileServerForTesting(cfs)

	server := &tremplinServer{
		lxd:                         lxd,
		upgradeStatus:               *NewTransactionMap(),
		listenerClient:              listener,
		upgradeClientUpdateInterval: 5 * time.Millisecond,
	}

	return server, lxd, op
}

func contains(arr []string, needle string) bool {
	for _, line := range arr {
		if line == needle {
			return true
		}
	}
	return false
}

func TestStartUpgradeContainerLimitOneInProgressPerContainer(t *testing.T) {
	done := make(chan bool)
	callback := func(status *pb.UpgradeContainerProgress) {
		done <- true
	}
	// Our upgrade sleeps for a very long time in the background, we don't wait for or need it to finish so it's fine.
	// This needs to be long enough that the second Start runs before the first finishes.
	server, _, _ := makeStubs(0.0, 1*time.Minute, callback)

	status, msg := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, got status %v and message %s", status, msg)
	}

	status, msg = server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_ALREADY_RUNNING {
		t.Fatalf("Failed to correctly fail when an upgrade is already running, got status %v and message %s", status, msg)
	}

	status, msg = server.startUpgradeContainer("test2", pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to allow multiple upgrades on separate containers, got status %v and message %s", status, msg)
	}
	<-done
}

// If the upgrade fails we should be able to try upgrading again.
func TestStartUpgradeContainerCanRetryAfterFailure(t *testing.T) {
	done := make(chan bool)
	callback := func(status *pb.UpgradeContainerProgress) {
		if status.Status != pb.UpgradeContainerProgress_IN_PROGRESS {
			done <- true
		}
	}

	server, lxd, operation := makeStubs(0.0, 0*time.Millisecond, callback)

	// Immediate failure.
	lxd.execError = errors.New("I'm a test error :)")
	status, _ := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_FAILED {
		t.Fatal("StartUpgrade didn't fail when it should've failed")
	}

	// Eventual failure.
	lxd.execError = nil
	operation.apiOperation.Metadata["return"] = 1.0
	status, msg := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start on retry after immediate failure, got status %v and message %s", status, msg)
	}

	// Wait until the above completes.
	<-done
	status, msg = server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_BUSTER)
	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start on retry after eventual failure, got status %v and message %s", status, msg)
	}
}

func TestUpgradeContainerVersionValidation(t *testing.T) {
	server, _, _ := makeStubs(127.0, 0*time.Millisecond, nil)

	tables := []struct {
		codename     string
		to           pb.UpgradeContainerRequest_Version
		expected     pb.UpgradeContainerResponse_Status
		expectedArgs []string
	}{
		{"", pb.UpgradeContainerRequest_DEBIAN_BUSTER, pb.UpgradeContainerResponse_NOT_SUPPORTED, []string{}},
		{"bookworm/sid", pb.UpgradeContainerRequest_DEBIAN_BUSTER, pb.UpgradeContainerResponse_NOT_SUPPORTED, []string{}},
		{"stretch", pb.UpgradeContainerRequest_UNKNOWN, pb.UpgradeContainerResponse_NOT_SUPPORTED, []string{}},
		{"buster", pb.UpgradeContainerRequest_UNKNOWN, pb.UpgradeContainerResponse_NOT_SUPPORTED, []string{}},
		{"bullseye", pb.UpgradeContainerRequest_UNKNOWN, pb.UpgradeContainerResponse_NOT_SUPPORTED, []string{}},
		{"stretch", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerResponse_NOT_SUPPORTED, []string{}},
		{"buster", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerResponse_NOT_SUPPORTED, []string{}},
		{"bullseye", pb.UpgradeContainerRequest_DEBIAN_STRETCH, pb.UpgradeContainerResponse_NOT_SUPPORTED, []string{}},
		{"stretch", pb.UpgradeContainerRequest_DEBIAN_BUSTER, pb.UpgradeContainerResponse_STARTED, []string{upgradeScriptPath, "DEBIAN_BUSTER"}},
		{"buster", pb.UpgradeContainerRequest_DEBIAN_BUSTER, pb.UpgradeContainerResponse_STARTED, []string{upgradeScriptPath}},
		{"bullseye", pb.UpgradeContainerRequest_DEBIAN_BUSTER, pb.UpgradeContainerResponse_NOT_SUPPORTED, []string{}},
		{"stretch", pb.UpgradeContainerRequest_DEBIAN_BULLSEYE, pb.UpgradeContainerResponse_STARTED, []string{upgradeScriptPath, "DEBIAN_BUSTER", "DEBIAN_BULLSEYE"}},
		{"buster", pb.UpgradeContainerRequest_DEBIAN_BULLSEYE, pb.UpgradeContainerResponse_STARTED, []string{upgradeScriptPath, "DEBIAN_BULLSEYE"}},
		{"bullseye", pb.UpgradeContainerRequest_DEBIAN_BULLSEYE, pb.UpgradeContainerResponse_STARTED, []string{upgradeScriptPath}},
	}

	for _, table := range tables {
		server.lxd.(*lxdStub).lastExec = nil
		server.lxd.(*lxdStub).osRelease = makeOsRelease(table.codename)
		status, err := server.startUpgradeContainer(fmt.Sprintf("%s -> %s", table.codename, table.to.String()), table.to)
		if status != table.expected {
			t.Errorf("Status of (%s -> %s) was incorrect, got: %v, want: %v.", table.codename, table.to.String(), status, table.expected)
			t.Errorf("Response message: %q", err)
		}

		var actualArgs []string
		if server.lxd.(*lxdStub).lastExec != nil {
			actualArgs = server.lxd.(*lxdStub).lastExec.Command
		}

		argsCorrect := true
		var expectedArgs []string
		if len(table.expectedArgs) > 0 {
			cmdPrefix := []string{"setsid"}
			expectedArgs = append(cmdPrefix, table.expectedArgs...)
		} else {
			expectedArgs = table.expectedArgs
		}

		for i, _ := range expectedArgs {
			if expectedArgs[i] != actualArgs[i] {
				argsCorrect = false
				break
			}
		}
		if !argsCorrect || len(expectedArgs) != len(actualArgs) {
			t.Errorf("(%s -> %d): Ran unexpected command %v and not %v", table.codename, table.to, actualArgs, expectedArgs)
		}
	}
}

func TestUpgradeContainerSendsInProgressMessages(t *testing.T) {
	statusChannel := make(chan *pb.UpgradeContainerProgress)
	callback := func(status *pb.UpgradeContainerProgress) {
		statusChannel <- status
		fmt.Println(status.ProgressMessages)
	}

	// Our upgrade sleeps for a very long time in the background, we don't wait for or need it to finish so it's fine.
	// This needs to be long enough that we see an in-progress message get sent.
	server, _, _ := makeStubs(0.0, 1*time.Minute, callback)

	status, msg := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_BUSTER)

	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, got %v and message %s", status, msg)
	}
	s := <-statusChannel
	if s.Status != pb.UpgradeContainerProgress_IN_PROGRESS {
		t.Fatalf("Didn't get in-progress message, got %v", s)
	}
	if len(s.ProgressMessages) == 0 {
		t.Fatal("Didn't get any progress messages")
	}
}

func TestUpgradeContainerSendsSuccessOnSuccessfulEnd(t *testing.T) {
	ch := make(chan *pb.UpgradeContainerProgress)
	callback := func(status *pb.UpgradeContainerProgress) {
		if status.Status != pb.UpgradeContainerProgress_IN_PROGRESS {
			ch <- status
		}
	}

	server, _, _ := makeStubs(0.0, 0*time.Millisecond, callback)

	status, msg := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_BUSTER)

	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, got %v and message %s", status, msg)
	}
	s := <-ch
	if s.Status != pb.UpgradeContainerProgress_SUCCEEDED {
		t.Fatalf("Didn't get success message, got %v", s)
	}
	if !contains(s.ProgressMessages, "Done message") {
		t.Fatalf("Didn't see expected end message, only saw: %v", s.ProgressMessages)
	}
}

func TestUpgradeContainerSendsFailureOnUnsuccessfulEnd(t *testing.T) {
	statusChannel := make(chan pb.UpgradeContainerProgress_Status)
	callback := func(status *pb.UpgradeContainerProgress) {
		if status.Status != pb.UpgradeContainerProgress_IN_PROGRESS {
			statusChannel <- status.Status
		}
	}

	server, _, _ := makeStubs(127.0, 0*time.Millisecond, callback)

	status, msg := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_BUSTER)

	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, got %v and message %s", status, msg)
	}
	if s := <-statusChannel; s != pb.UpgradeContainerProgress_FAILED {
		t.Fatalf("Didn't get failure message, got %v", s)
	}
}

func TestUpgradeContainerSendsFailureOnLxdError(t *testing.T) {
	statusChannel := make(chan pb.UpgradeContainerProgress_Status)
	callback := func(status *pb.UpgradeContainerProgress) {
		if status.Status != pb.UpgradeContainerProgress_IN_PROGRESS {
			statusChannel <- status.Status
		}
	}

	server, _, op := makeStubs(0.0, 0*time.Millisecond, callback)
	op.waitError = errors.New("I'm a test error :)")

	status, msg := server.startUpgradeContainer("test1", pb.UpgradeContainerRequest_DEBIAN_BUSTER)

	if status != pb.UpgradeContainerResponse_STARTED {
		t.Fatalf("Failed to start, got %v and message %s", status, msg)
	}
	if s := <-statusChannel; s != pb.UpgradeContainerProgress_FAILED {
		t.Fatalf("Didn't get failure message, got %v", s)
	}
}

func TestCancelNotRunning(t *testing.T) {
	ch := make(chan *pb.UpgradeContainerProgress)
	callback := func(status *pb.UpgradeContainerProgress) {
		if status.Status != pb.UpgradeContainerProgress_IN_PROGRESS {
			ch <- status
		}
	}
	server, _, _ := makeStubs(0.0, 0*time.Millisecond, callback)

	status, _ := server.cancelUpgradeContainer("test")

	if status != pb.CancelUpgradeContainerResponse_NOT_RUNNING {
		t.Errorf("Unexpected status, got: %v, want: %v.", status, pb.CancelUpgradeContainerResponse_NOT_RUNNING)
	}
}
