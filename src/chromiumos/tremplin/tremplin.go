// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	pb "chromiumos/vm_tools/tremplin_proto"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/ioprogress"
	"github.com/lxc/lxd/shared/osarch"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v2"
)

const (
	backupSnapshot      = "rootfs-backup"
	importContainerName = "rootfs-import"
	shiftSnapshot       = "rootfs-shift"

	lingerPath         = "/var/lib/systemd/linger"
	primaryUserID      = 1000
	chronosAccessID    = 1001
	androidRootID      = 655360
	androidEverybodyID = 665357
)

// downloadRegexp extracts the download type and progress percentage from
// download operation metadata.
var downloadRegexp *regexp.Regexp

func init() {
	// Example matches:
	// "metadata: 100% (5.23MB/s)" matches ("metadata", "100")
	// "rootfs: 23% (358.09kB/s)" matches ("rootfs", "23")
	downloadRegexp = regexp.MustCompile("([[:alpha:]]+): ([[:digit:]]+)% [0-9A-Za-z /.()]*$")
}

// getContainerName converts an LXD source path (/1.0/containers/foo) to a container name.
func getContainerName(s string) (string, error) {
	components := strings.Split(s, "/")
	// Expected components are: "", "1.0", "containers", "<container name>".
	if len(components) != 4 {
		return "", fmt.Errorf("invalid source path: %q", s)
	}
	if components[2] != "containers" {
		return "", fmt.Errorf("source path is not a container: %q", s)
	}
	return components[3], nil
}

// getDownloadPercentage extracts the download progress (as a percentage)
// from an api.Operation's Metadata map.
func getDownloadPercentage(progress string) (int32, error) {
	matches := downloadRegexp.FindStringSubmatch(progress)
	if matches == nil {
		return 0, fmt.Errorf("didn't find download status in %q", progress)
	}

	downloadPercent, err := strconv.ParseInt(matches[2], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to convert download percent to int: %q", matches[2])
	}

	// Count metadata download as 0% of the total, since the entire rootfs still
	// needs to be downloaded.
	if matches[1] == "metadata" {
		downloadPercent = 0
	}

	return int32(downloadPercent), nil
}

// idRemapRequired examines the last and next idmaps for a container and checks
// if the container rootfs will require a remap when it next starts.
func idRemapRequired(c *api.Container) (bool, error) {
	lastIdmap, ok := c.ExpandedConfig["volatile.last_state.idmap"]
	if !ok {
		return false, fmt.Errorf("no volatile.last_state.idmap key for container %s", c.Name)
	}
	nextIdmap, ok := c.ExpandedConfig["volatile.idmap.next"]
	if !ok {
		return false, fmt.Errorf("no volatile.idmap.next key for container %s", c.Name)
	}

	// The idmap configs are JSON-encoded arrays of LXD idmap entries.
	unmarshaledLastIdmap := []interface{}{}
	if err := json.Unmarshal([]byte(lastIdmap), &unmarshaledLastIdmap); err != nil {
		return false, err
	}

	unmarshaledNextIdmap := []interface{}{}
	if err := json.Unmarshal([]byte(nextIdmap), &unmarshaledNextIdmap); err != nil {
		return false, err
	}

	// A remap is required only if the last and next idmaps don't match.
	return !reflect.DeepEqual(unmarshaledLastIdmap, unmarshaledNextIdmap), nil
}

// tremplinServer is used to implement the gRPC tremplin.Server.
type tremplinServer struct {
	lxd            lxd.ContainerServer
	grpcServer     *grpc.Server
	listenerClient pb.TremplinListenerClient
	milestone      int
	timezoneName   string
}

// execProgram runs a program in a container to completion, capturing its
// return value, stdout, and stderr.
func (s *tremplinServer) execProgram(containerName string, args []string) (ret int, stdout string, stderr string, err error) {
	req := api.ContainerExecPost{
		Command:     args,
		WaitForWS:   true,
		Interactive: false,
	}

	stdoutSink := &stdioSink{}
	stderrSink := &stdioSink{}
	execArgs := &lxd.ContainerExecArgs{
		Stdin:  &stdioSink{},
		Stdout: stdoutSink,
		Stderr: stderrSink,
	}

	op, err := s.lxd.ExecContainer(containerName, req, execArgs)
	if err != nil {
		return 0, "", "", err
	}

	if err = op.Wait(); err != nil {
		return 0, "", "", err
	}
	opAPI := op.Get()

	retVal, ok := opAPI.Metadata["return"].(float64)
	if !ok {
		return 0, "", "", fmt.Errorf("return value for %q is not a float64", args[0])
	}
	return int(retVal), stdoutSink.String(), stderrSink.String(), nil
}

// createSnapshot creates a snapshot with snapshotName for containerName.
// Any existing snapshot with the existing snapshotName is deleted first.
func (s *tremplinServer) createSnapshot(containerName, snapshotName string) error {
	names, err := s.lxd.GetContainerSnapshotNames(containerName)
	if err != nil {
		return fmt.Errorf("failed to get container snapshot names: %v", err)
	}

	// Delete any existing snapshot with the same name.
	for _, name := range names {
		if name == snapshotName {
			op, err := s.lxd.DeleteContainerSnapshot(containerName, snapshotName)
			if err != nil {
				return fmt.Errorf("failed to delete existing snapshot %s: %v", snapshotName, err)
			}
			if err = op.Wait(); err != nil {
				return fmt.Errorf("failed to wait for snapshot %s deletion: %v", snapshotName, err)
			}
			opAPI := op.Get()
			if opAPI.StatusCode != api.Success {
				return fmt.Errorf("snapshot %s deletion failed: %s", snapshotName, opAPI.Err)
			}

			break
		}
	}

	op, err := s.lxd.CreateContainerSnapshot(containerName, api.ContainerSnapshotsPost{
		Name:     snapshotName,
		Stateful: false,
	})
	if err != nil {
		return fmt.Errorf("failed to create container snapshot %s: %v", snapshotName, err)
	}
	if err = op.Wait(); err != nil {
		return fmt.Errorf("failed to wait for snapshot %s creation: %v", snapshotName, err)
	}
	opAPI := op.Get()
	if opAPI.StatusCode != api.Success {
		return fmt.Errorf("snapshot %s creation failed: %s", snapshotName, opAPI.Err)
	}

	return nil
}

func (s *tremplinServer) startContainer(containerName string, remap bool) {
	req := &pb.ContainerStartProgress{
		ContainerName: containerName,
	}

	// The host must be informed of the final outcome, so ensure it's updated
	// on every exit path.
	defer func() {
		if req == nil {
			return
		}
		_, err := s.listenerClient.UpdateStartStatus(context.Background(), req)
		if err != nil {
			log.Printf("Could not update start status on host: %v", err)
			return
		}
	}()

	if remap {
		log.Printf("Snapshotting container %s to prepare for id remap", containerName)
		if err := s.createSnapshot(containerName, shiftSnapshot); err != nil {
			req.Status = pb.ContainerStartProgress_FAILED
			req.FailureReason = err.Error()
			return
		}
	}

	reqState := api.ContainerStatePut{
		Action:  "start",
		Timeout: -1,
	}
	op, err := s.lxd.UpdateContainerState(containerName, reqState, "")
	if err != nil {
		req.Status = pb.ContainerStartProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to start container: %v", err)
		return
	}

	if err = op.Wait(); err != nil {
		req.Status = pb.ContainerStartProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to wait for container startup: %v", err)
		return
	}
	opAPI := op.Get()

	switch opAPI.StatusCode {
	case api.Success:
		req.Status = pb.ContainerStartProgress_STARTED
	case api.Cancelled:
		req.Status = pb.ContainerStartProgress_CANCELLED
	case api.Failure:
		req.Status = pb.ContainerStartProgress_FAILED
		req.FailureReason = opAPI.Err
	}
}

func (s *tremplinServer) handleCreateOperation(op api.Operation) {
	containers := op.Resources["containers"]

	if len(containers) != 1 {
		log.Printf("Got %v containers instead of 1", len(containers))
		return
	}

	name, err := getContainerName(containers[0])
	if err != nil {
		log.Printf("Failed to get container name for operation: %v", err)
		return
	}

	req := &pb.ContainerCreationProgress{
		ContainerName: name,
	}

	switch op.StatusCode {
	case api.Pending:
		// The operation will only be here a short time before transitioning to
		// Running. Don't bother informing the host since there's not anything
		// it can do yet.
		return
	case api.Success:
		req.Status = pb.ContainerCreationProgress_CREATED
	case api.Running:
		req.Status = pb.ContainerCreationProgress_DOWNLOADING
		progress, ok := op.Metadata["download_progress"].(string)
		if ok {
			downloadPercent, err := getDownloadPercentage(progress)
			if err != nil {
				log.Printf("Failed to parse download percentage: %v", err)
				return
			}
			req.DownloadProgress = downloadPercent
		} else {
			return
		}
	case api.Cancelled, api.Failure:
		req.Status = pb.ContainerCreationProgress_FAILED
		req.FailureReason = op.Err
	default:
		req.Status = pb.ContainerCreationProgress_UNKNOWN
		req.FailureReason = fmt.Sprintf("unhandled create status: %s", op.Status)
	}

	_, err = s.listenerClient.UpdateCreateStatus(context.Background(), req)
	if err != nil {
		log.Printf("Could not update create status on host: %v", err)
		return
	}
}

// CreateContainer implements tremplin.CreateContainer.
func (s *tremplinServer) CreateContainer(ctx context.Context, in *pb.CreateContainerRequest) (*pb.CreateContainerResponse, error) {
	log.Printf("Received CreateContainer RPC: %s", in.ContainerName)

	response := &pb.CreateContainerResponse{}

	container, _, _ := s.lxd.GetContainer(in.ContainerName)
	if container != nil {
		response.Status = pb.CreateContainerResponse_EXISTS
		return response, nil
	}

	imageServerUrl := strings.Replace(in.ImageServer, "%d", strconv.Itoa(s.milestone), 1)

	imageServer, err := lxd.ConnectSimpleStreams(imageServerUrl, nil)
	if err != nil {
		response.Status = pb.CreateContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to connect to simplestreams image server: %v", err)
		return response, nil
	}

	alias, _, err := imageServer.GetImageAlias(in.ImageAlias)
	if err != nil {
		response.Status = pb.CreateContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to get alias: %v", err)
		return response, nil
	}

	image, _, err := imageServer.GetImage(alias.Target)
	if err != nil {
		response.Status = pb.CreateContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to get image for alias: %v", err)
		return response, nil
	}

	containersPost := api.ContainersPost{
		Name: in.ContainerName,
		Source: api.ContainerSource{
			Type:  "image",
			Alias: alias.Name,
		},
	}
	op, err := s.lxd.CreateContainerFromImage(imageServer, *image, containersPost)
	if err != nil {
		response.Status = pb.CreateContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to create container from image: %v", err)
		return response, nil
	}

	_, err = op.AddHandler(func(op api.Operation) { s.handleCreateOperation(op) })
	if err != nil {
		log.Fatal("Failed to add create operation handler: ", err)
	}

	response.Status = pb.CreateContainerResponse_CREATING

	return response, nil
}

type bindMount struct {
	name    string
	content string
	source  string
	dest    string
}

// DeleteContainer implements tremplin.DeleteContainer.
func (s *tremplinServer) DeleteContainer(ctx context.Context, in *pb.DeleteContainerRequest) (*pb.DeleteContainerResponse, error) {
	log.Printf("Received DeleteContainer RPC: %s", in.ContainerName)

	response := &pb.DeleteContainerResponse{}

	container, _, err := s.lxd.GetContainer(in.ContainerName)
	if container == nil {
		response.Status = pb.DeleteContainerResponse_DOES_NOT_EXIST
		return response, nil
	}
	if err != nil {
		response.Status = pb.DeleteContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to find container: %v", err)
		return response, nil
	}

	if container.StatusCode == api.Running {
		reqState := api.ContainerStatePut{
			Action:  "stop",
			Timeout: -1,
			Force:   true,
		}
		op, err := s.lxd.UpdateContainerState(container.Name, reqState, "")

		if err != nil {
			response.Status = pb.DeleteContainerResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to stop container: %v", err)
			return response, nil
		}

		_, err = op.AddHandler(func(op api.Operation) { s.handleStopOperation(container.Name, op) })

		if err != nil {
			response.Status = pb.DeleteContainerResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to add stop operation handler: %v", err)
			return response, nil
		}
	} else {
		err := s.startDeleteOperation(container.Name)
		if err != nil {
			response.Status = pb.DeleteContainerResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to delete container: %v", err)
			return response, nil
		}
	}

	response.Status = pb.DeleteContainerResponse_DELETING
	return response, nil
}

func (s *tremplinServer) handleStopOperation(containerName string, op api.Operation) {
	req := &pb.ContainerDeletionProgress{
		ContainerName: containerName,
	}

	switch op.StatusCode {
	case api.Pending, api.Running:
		return
	case api.Success:
		err := s.startDeleteOperation(containerName)
		if err == nil {
			return
		}
		req.Status = pb.ContainerDeletionProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to stop container: %v", err)
	case api.Cancelled:
		req.Status = pb.ContainerDeletionProgress_CANCELLED
		req.FailureReason = op.Err
	case api.Failure:
		req.Status = pb.ContainerDeletionProgress_FAILED
		req.FailureReason = op.Err
	default:
		req.Status = pb.ContainerDeletionProgress_UNKNOWN
		req.FailureReason = fmt.Sprintf("unhandled stop status: %s, %s", op.Status, op.Err)
	}

	_, err := s.listenerClient.UpdateDeletionStatus(context.Background(), req)
	if err != nil {
		log.Printf("Could not update deletion status on host: %v", err)
	}
}

func (s *tremplinServer) startDeleteOperation(containerName string) (err error) {
	op, err := s.lxd.DeleteContainer(containerName)

	if err != nil {
		return err
	}

	if _, err := op.AddHandler(func(op api.Operation) { s.handleDeleteOperation(containerName, op) }); err != nil {
		return err
	}
	return nil
}

func (s *tremplinServer) handleDeleteOperation(containerName string, op api.Operation) {
	req := &pb.ContainerDeletionProgress{
		ContainerName: containerName,
	}

	switch op.StatusCode {
	case api.Pending, api.Running:
		return
	case api.Success:
		req.Status = pb.ContainerDeletionProgress_DELETED
	case api.Cancelled:
		req.Status = pb.ContainerDeletionProgress_CANCELLED
		req.FailureReason = op.Err
	case api.Failure:
		req.Status = pb.ContainerDeletionProgress_FAILED
		req.FailureReason = op.Err
	default:
		req.Status = pb.ContainerDeletionProgress_UNKNOWN
		req.FailureReason = fmt.Sprintf("unhandled deletion status: %s, %s", op.Status, op.Err)
	}

	if _, err := s.listenerClient.UpdateDeletionStatus(context.Background(), req); err != nil {
		log.Printf("Could not update deletion status on host: %v", err)
	}
}

// StartContainer implements tremplin.StartContainer.
func (s *tremplinServer) StartContainer(ctx context.Context, in *pb.StartContainerRequest) (*pb.StartContainerResponse, error) {
	log.Printf("Received StartContainer RPC: %s", in.ContainerName)

	response := &pb.StartContainerResponse{}

	container, etag, err := s.lxd.GetContainer(in.ContainerName)
	if err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to find container: %v", err)
		return response, nil
	}

	if container.StatusCode == api.Running {
		response.Status = pb.StartContainerResponse_RUNNING
		return response, nil
	}

	// Prepare SSH keys, token, and apt config to bind-mount in.
	// Clear out all existing devices for the container.
	containerPut := container.Writable()
	containerPut.Devices = map[string]map[string]string{}
	err = os.MkdirAll(fmt.Sprintf("/run/sshd/%s", container.Name), 0644)
	if err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to create ssh key dir: %v", err)
		return response, nil
	}
	bindMounts := []bindMount{
		{
			name:    "container_token",
			content: in.Token,
			source:  fmt.Sprintf("/run/tokens/%s_token", container.Name),
			dest:    "/dev/.container_token",
		},
		{
			name:    "ssh_authorized_keys",
			content: in.HostPublicKey,
			source:  fmt.Sprintf("/run/sshd/%s/authorized_keys", container.Name),
			dest:    "/dev/.ssh/ssh_authorized_keys",
		},
		{
			name:    "ssh_host_key",
			content: in.ContainerPrivateKey,
			source:  fmt.Sprintf("/run/sshd/%s/ssh_host_key", container.Name),
			dest:    "/dev/.ssh/ssh_host_key",
		},
	}

	osRelease, err := getGuestOSRelease(s.lxd, container.Name)
	if err == nil {
		if osRelease.id == "debian" {
			args := lxd.ContainerFileArgs{
				Content:   strings.NewReader(createAptSourceList(s.milestone)),
				UID:       0,
				GID:       0,
				Mode:      0644,
				Type:      "file",
				WriteMode: "overwrite",
			}
			err = s.lxd.CreateContainerFile(container.Name, "/etc/apt/sources.list.d/cros.list", args)
			if err != nil {
				log.Print("Failed to update guest cros.list:", err)
			}
		}
	} else {
		log.Printf("Could not identify container %q guest distro: %v", container.Name, err)
	}

	for _, b := range bindMounts {
		// Disregard bind mounts without values.
		if b.content == "" {
			continue
		}

		err = ioutil.WriteFile(b.source, []byte(b.content), 0644)
		if err != nil {
			response.Status = pb.StartContainerResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to write %q: %v", b.source, err)
			return response, nil
		}

		containerPut.Devices[b.name] = map[string]string{
			"source": b.source,
			"path":   b.dest,
			"type":   "disk",
		}
	}

	op, err := s.lxd.UpdateContainer(container.Name, containerPut, etag)
	if err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to set up devices: %v", err)
		return response, nil
	}
	if err = op.Wait(); err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to wait for container update: %v", err)
		return response, nil
	}
	opAPI := op.Get()
	if opAPI.StatusCode != api.Success {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to update container: %v", err)
		return response, nil
	}

	// We've updated the container, so refresh the local copy.
	container, etag, err = s.lxd.GetContainer(in.ContainerName)
	if err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to get updated container: %v", err)
		return response, nil
	}

	remapRequired, err := idRemapRequired(container)
	if err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to check if id remap required: %v", err)
		return response, nil
	}

	if in.Async {
		go s.startContainer(container.Name, remapRequired)

		if remapRequired {
			response.Status = pb.StartContainerResponse_REMAPPING

		} else {
			response.Status = pb.StartContainerResponse_STARTING
		}

		return response, nil
	}

	// TODO(smbarber): Remove once async codepath is default.
	reqState := api.ContainerStatePut{
		Action:  "start",
		Timeout: -1,
	}
	op, err = s.lxd.UpdateContainerState(container.Name, reqState, "")
	if err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to start container: %v", err)
		return response, nil
	}

	if err = op.Wait(); err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to wait for container startup: %v", err)
		return response, nil
	}
	opAPI = op.Get()

	switch opAPI.StatusCode {
	case api.Success:
		response.Status = pb.StartContainerResponse_STARTED
		if s.timezoneName != "" {
			// In case the container was off when we set the timezone, we try
			// to set the timezone again when the container starts up.
			ret, _, _, err := s.execProgram(container.Name, []string{"timedatectl", "set-timezone", s.timezoneName})
			if err == nil && ret == 0 {
				// Success! Unset the TZ variable as it takes precedence over the timedatectl setting.
				delete(container.Config, "environment.TZ")
				s.lxd.UpdateContainer(container.Name, container.Writable(), "")
			}
		}
	case api.Cancelled, api.Failure:
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = opAPI.Err
	}

	return response, nil
}

// GetContainerUsername implements tremplin.GetContainerUsername.
func (s *tremplinServer) GetContainerUsername(ctx context.Context, in *pb.GetContainerUsernameRequest) (*pb.GetContainerUsernameResponse, error) {
	log.Printf("Received GetContainerUsername RPC: %s", in.ContainerName)

	response := &pb.GetContainerUsernameResponse{}

	_, _, err := s.lxd.GetContainer(in.ContainerName)
	if err != nil {
		response.Status = pb.GetContainerUsernameResponse_CONTAINER_NOT_FOUND
		response.FailureReason = fmt.Sprintf("failed to find container: %v", err)
		return response, nil
	}

	pd, err := NewPasswdDatabase(s.lxd, in.ContainerName)
	if err != nil {
		response.Status = pb.GetContainerUsernameResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to get container passwd db: %v", err)
		return response, nil
	}

	p := pd.PasswdForUid(primaryUserID)
	if p == nil {
		response.Status = pb.GetContainerUsernameResponse_USER_NOT_FOUND
		response.FailureReason = "failed to find user for uid"
		return response, nil
	}

	response.Username = p.Name
	response.Homedir = p.Homedir
	response.Status = pb.GetContainerUsernameResponse_SUCCESS

	return response, nil
}

// SetUpUser implements tremplin.SetUpUser.
func (s *tremplinServer) SetUpUser(ctx context.Context, in *pb.SetUpUserRequest) (*pb.SetUpUserResponse, error) {
	log.Printf("Received SetUpUser RPC: %s", in.ContainerName)

	response := &pb.SetUpUserResponse{}

	pd, err := NewPasswdDatabase(s.lxd, in.ContainerName)
	if err != nil {
		response.Status = pb.SetUpUserResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to get container passwd db: %v", err)
		return response, nil
	}

	p := pd.PasswdForUid(primaryUserID)
	if p != nil {
		response.Username = p.Name
	} else {
		response.Username = in.ContainerUsername
	}

	if err := pd.EnsureUserExists(response.Username, primaryUserID, true); err != nil {
		response.Status = pb.SetUpUserResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to ensure primary user exists: %v", err)
		return response, nil
	}

	if err := pd.EnsureUserExists("android-root", androidRootID, false); err != nil {
		response.Status = pb.SetUpUserResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to create android-root: %v", err)
		return response, nil
	}

	pd.EnsureGroupExists("android-everybody", androidEverybodyID)
	pd.EnsureGroupExists("chronos-access", chronosAccessID)

	groups := []struct {
		name     string
		required bool
	}{
		{"android-everybody", true},
		{"chronos-access", true},
		{"audio", false},
		{"cdrom", false},
		{"dialout", false},
		{"floppy", false},
		{"plugdev", false},
		{"sudo", false},
		{"users", false},
		{"video", false},
	}

	for _, group := range groups {
		err := pd.EnsureUserInGroup(response.Username, group.name)
		if err != nil && group.required {
			response.Status = pb.SetUpUserResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to add user to required group %q: %v", group.name, err)
			return response, nil
		}
	}

	if err := pd.Save(); err != nil {
		response.Status = pb.SetUpUserResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to save passwd db: %v", err)
		return response, nil
	}

	container, _, err := s.lxd.GetContainer(in.ContainerName)
	if err != nil {
		response.Status = pb.SetUpUserResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to find container: %v", err)
		return response, nil
	}

	// Enable loginctl linger for the target user. If running, use loginctl to
	// kick and start a user session.
	if err := pd.lxd.CreateContainerFile(in.ContainerName, lingerPath, lxd.ContainerFileArgs{
		UID:  0,
		GID:  0,
		Mode: 0755,
		Type: "directory",
	}); err != nil {
		response.Status = pb.SetUpUserResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to create linger dir: %v", err)
		return response, nil
	}
	userLingerPath := path.Join(lingerPath, response.Username)
	if err := pd.lxd.CreateContainerFile(in.ContainerName, userLingerPath, lxd.ContainerFileArgs{
		UID:  0,
		GID:  0,
		Mode: 0644,
		Type: "file",
	}); err != nil {
		response.Status = pb.SetUpUserResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to create linger file: %v", err)
		return response, nil
	}

	if container.StatusCode == api.Running {
		ret, _, stderr, err := s.execProgram(in.ContainerName,
			[]string{"loginctl", "enable-linger", response.Username})
		if err != nil {
			response.Status = pb.SetUpUserResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to run loginctl: %v", err)
			return response, nil
		}
		if ret != 0 {
			response.Status = pb.SetUpUserResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to enable linger: %s", stderr)
			return response, nil
		}
	}

	response.Status = pb.SetUpUserResponse_SUCCESS

	return response, nil
}

// GetContainerInfo implements tremplin.GetContainerInfo.
func (s *tremplinServer) GetContainerInfo(ctx context.Context, in *pb.GetContainerInfoRequest) (*pb.GetContainerInfoResponse, error) {
	log.Printf("Received GetContainerInfo RPC: %s", in.ContainerName)

	response := &pb.GetContainerInfoResponse{}

	c, _, err := s.lxd.GetContainerState(in.ContainerName)
	if err != nil {
		response.Status = pb.GetContainerInfoResponse_NOT_FOUND
		response.FailureReason = fmt.Sprintf("failed to find container: %v", err)
		return response, nil
	}
	if c.StatusCode != api.Running {
		response.Status = pb.GetContainerInfoResponse_STOPPED
		response.FailureReason = fmt.Sprintf("container not running, status is: %d", c.StatusCode)
		return response, nil
	}

	n, ok := c.Network["eth0"]
	if !ok {
		response.Status = pb.GetContainerInfoResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to get eth0 for container %q", in.ContainerName)
		return response, nil
	}

	for _, addr := range n.Addresses {
		if addr.Family == "inet" {
			ip := net.ParseIP(addr.Address)
			if ip == nil {
				response.Status = pb.GetContainerInfoResponse_FAILED
				response.FailureReason = fmt.Sprintf("failed to parse ipv4 address for container %q", in.ContainerName)
				return response, nil
			}
			// Yes, this should be big endian. I don't know why it's flipped.
			response.Ipv4Address = binary.LittleEndian.Uint32(ip.To4())
			break
		}
	}

	if response.Ipv4Address == 0 {
		response.Status = pb.GetContainerInfoResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to find ipv4 address for container %q", in.ContainerName)
		return response, nil
	}

	response.Status = pb.GetContainerInfoResponse_RUNNING
	return response, nil
}

// SetTimezone implements tremplin.SetTimezone.
func (s *tremplinServer) SetTimezone(ctx context.Context, in *pb.SetTimezoneRequest) (*pb.SetTimezoneResponse, error) {
	log.Printf("Received SetTimezone RPC: %s", in.TimezoneName)

	response := &pb.SetTimezoneResponse{}
	s.timezoneName = in.TimezoneName
	for _, name := range in.ContainerNames {
		container, _, err := s.lxd.GetContainer(name)
		if err != nil {
			response.FailureReasons = append(response.FailureReasons, fmt.Sprintf("could not get container %s: %v", container.Name, err))
			continue
		}
		var errors []string
		// First option, use timedatectl.
		ret, _, _, err := s.execProgram(container.Name, []string{"timedatectl", "set-timezone", in.TimezoneName})
		if err == nil && ret == 0 {
			response.Successes++
			// Attempt to unset TZ env variable in case it was set earlier and is now incorrect.
			delete(container.Config, "environment.TZ")
			s.lxd.UpdateContainer(container.Name, container.Writable(), "")
			continue
		}
		errors = append(errors, fmt.Sprintf("setting timezone by name failed: (error: %v, return code: %d)", err, ret))

		// Second option, set the TZ environment variable for this particular container.
		if in.PosixTzString == "" {
			errors = append(errors, fmt.Sprintf("setting timezone by TZ variable failed: no POSIX TZ string provided"))
		} else {
			container.Config["environment.TZ"] = in.PosixTzString
			operation, err := s.lxd.UpdateContainer(container.Name, container.Writable(), "")
			if err == nil {
				// UpdateContainer is relatively fast so no need to run asynchronously.
				err := operation.Wait()
				if err == nil {
					response.Successes++
					continue
				}
			}
			errors = append(errors, fmt.Sprintf("setting timezone by TZ variable failed: %v", err))
		}

		response.FailureReasons = append(response.FailureReasons, fmt.Sprintf("container %s: %s", container.Name, strings.Join(errors, ", ")))
	}

	return response, nil
}

// seekingProgressWriter adds a nop Seek function to ioprogress.ProgressWriter
// and allows it to be used for the export download in ImageFileRequest.
type seekingProgressWriter struct {
	*ioprogress.ProgressWriter
}

func (w *seekingProgressWriter) Seek(offset int64, whence int) (int64, error) {
	return offset, nil
}

// getProgress gets stage, percent, speed from the operation metadata.
func getProgress(op api.Operation) (stage string, percent uint32, speed uint64, ok bool) {
	// Get 'progress' from Metadata as map[string]interface{}.
	progress, ok := op.Metadata["progress"]
	if !ok {
		return
	}
	progressMap, ok := progress.(map[string]interface{})
	if !ok {
		log.Printf("Could not convert progress map to map[string]interface{}, got: %v", reflect.TypeOf(progress))
		return
	}

	// Get 'stage', 'percent', 'speed' as strings.
	stageVal, stageOK := progressMap["stage"]
	percentVal, percentOK := progressMap["percent"]
	speedVal, speedOK := progressMap["speed"]
	ok = stageOK && percentOK && speedOK
	if !ok {
		log.Printf("Progress map found fields stage=%v, percent=%v, speed=%v", stageOK, percentOK, speedOK)
		return
	}
	stage, stageOK = stageVal.(string)
	percentStr, percentOK := percentVal.(string)
	speedStr, speedOK := speedVal.(string)
	ok = stageOK && percentOK && speedOK
	if !ok {
		log.Printf("Progress map could not convert fields to string, got stage=%v, percent=%v, speed=%v", reflect.TypeOf(stageVal), reflect.TypeOf(percentVal), reflect.TypeOf(speedVal))
		return
	}

	// Convert percent to uint32, speed to uint64.
	percent64, err := strconv.ParseUint(percentStr, 10, 32)
	if err != nil {
		ok = false
		log.Printf("Could not parse progress percent: %v", err)
	}
	percent = uint32(percent64)
	speed, err = strconv.ParseUint(speedStr, 10, 64)
	if err != nil {
		ok = false
		log.Printf("Could not parse progress speed: %v", err)
	}
	return
}

func (s *tremplinServer) exportContainer(containerName, exportPath string) {
	req := &pb.ContainerExportProgress{
		ContainerName: containerName,
	}

	// The host must be informed of the final outcome, so ensure it's updated
	// on every exit path.
	defer func() {
		if req == nil {
			return
		}
		_, err := s.listenerClient.UpdateExportStatus(context.Background(), req)
		if err != nil {
			log.Printf("Could not update export status on host: %v", err)
			return
		}
	}()

	// Create a snapshot for export.  It is OK if the container is running.
	if err := s.createSnapshot(containerName, backupSnapshot); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to create backup snapshot %s/%s: %v", containerName, backupSnapshot, err)
		return
	}

	// Publish.
	reqPublish := api.ImagesPost{
		Source: &api.ImagesPostSource{
			Type: "snapshot",
			Name: containerName + "/" + backupSnapshot,
		},
	}
	op, err := s.lxd.CreateImage(reqPublish, nil)
	if err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to publish snapshot %s/%s: %v", containerName, backupSnapshot, err)
		return
	}
	op.AddHandler(func(op api.Operation) {
		stage, percent, speed, ok := getProgress(op)
		if !ok {
			return
		}
		req.ProgressPercent = percent
		req.ProgressSpeed = speed
		switch stage {
		case "create_image_from_container_pack":
			req.Status = pb.ContainerExportProgress_EXPORTING_PACK
		default:
			log.Printf("Unknown CreateImage stage: %v", stage)
			return
		}
		_, err = s.listenerClient.UpdateExportStatus(context.Background(), req)
		if err != nil {
			log.Printf("Could not update CreateImage status on host: %v", err)
			return
		}
	})
	var fingerprint string
	size := int64(-1)
	// An error for wait is only a problem if fingerprint and size are not
	// returned.  If the image already exists, we can continue with export.
	err = op.Wait()
	if f, ok := op.Get().Metadata["fingerprint"]; ok {
		fingerprint = f.(string)
	}
	if s, ok := op.Get().Metadata["size"]; ok {
		size, _ = strconv.ParseInt(s.(string), 10, 64)
	}
	if fingerprint == "" || size == -1 {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed waiting to publish snapshot %s/%s: %v", containerName, backupSnapshot, err)
		return
	}

	// Export image.  If exportPath is a directory, then name file
	// <fingerprint>.tar.gz, otherwise use filename given.
	// We do not have priv to stat, so attempt to create a file with
	// the given path, if that fails, append <fingerprint>.tar.gz.
	exportFilename := filepath.Join("/mnt/shared", exportPath)
	exportFile, err := os.Create(exportFilename)
	if err != nil {
		// Assume Create failed because this is a directory.
		// Try <exportPath>/<fingerprint>.tar.gz.
		exportFilenameDir := filepath.Join(exportFilename, fingerprint+".tar.gz")
		var errDir error
		exportFile, errDir = os.Create(exportFilenameDir)
		if errDir != nil {
			req.Status = pb.ContainerExportProgress_FAILED
			req.FailureReason = fmt.Sprintf("failed to create export file as %s: %v, or as %s: %v", exportFilename, err, exportFilenameDir, errDir)
			return
		}
	}
	defer exportFile.Close()

	// Download the image.  We only need to supply ImageFileRequest.MetaFile
	// and not ImageFileRequest.RootfsFile.  The exported image is a unified
	// tarball with metadata and rootfs combined unless the LXD HTTP request
	// to get the image has "Content-Type: multipart/form-data".
	// https://github.com/lxc/lxd/blob/6802d7f73cf44fda6284fd8e657038e37d84e762/client/lxd_images.go#L197
	// Use a ProgressWriter as a wrapper for exportFile.
	log.Printf("Downloading image %s to %s, size=%d", fingerprint, exportFile.Name(), size)
	reqExport := lxd.ImageFileRequest{
		MetaFile: &seekingProgressWriter{&ioprogress.ProgressWriter{
			WriteCloser: exportFile,
			Tracker: &ioprogress.ProgressTracker{
				Length: size,
				Handler: func(percent, speed int64) {
					req.ProgressPercent = uint32(percent)
					req.ProgressSpeed = uint64(speed)
					req.Status = pb.ContainerExportProgress_EXPORTING_DOWNLOAD
					_, err = s.listenerClient.UpdateExportStatus(context.Background(), req)
					if err != nil {
						log.Printf("Could not update GetImageFile status on host: %v", err)
						return
					}
				},
			},
		}},
	}
	_, err = s.lxd.GetImageFile(fingerprint, reqExport)
	if err != nil {
		os.Remove(exportFilename)
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to get image file %s: %v", fingerprint, err)
		return
	}

	// Delete image.
	op, err = s.lxd.DeleteImage(fingerprint)
	if err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error deleting image %s: %v", fingerprint, err)
		return
	}
	if err = op.Wait(); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error waiting to delete image %s: %v", fingerprint, err)
		return
	}

	req.Status = pb.ContainerExportProgress_DONE
	log.Printf("ExportContainer done")
}

// ExportContainer implements tremplin.ExportContainer.
func (s *tremplinServer) ExportContainer(ctx context.Context, in *pb.ExportContainerRequest) (*pb.ExportContainerResponse, error) {
	log.Printf("Received ExportContainer RPC: %s %s", in.ContainerName, in.ExportPath)

	go s.exportContainer(in.ContainerName, in.ExportPath)
	response := &pb.ExportContainerResponse{
		Status: pb.ExportContainerResponse_EXPORTING,
	}
	return response, nil
}

// deleteContainer deletes a container if it exists.
func (s *tremplinServer) deleteContainer(containerName string) error {
	// Ignore any error from GetContainer.
	c, _, _ := s.lxd.GetContainer(containerName)
	if c == nil {
		log.Printf("Ignoring request to delete non-existent container %s", containerName)
		return nil
	}
	if c.StatusCode != 0 && c.StatusCode != api.Stopped {
		log.Printf("Force stopping container %s before deleting", containerName)
		reqState := api.ContainerStatePut{
			Action:  "stop",
			Timeout: -1,
			Force:   true,
		}
		op, err := s.lxd.UpdateContainerState(containerName, reqState, "")
		if err = op.Wait(); err != nil {
			return err
		}
		// Notify cicerone that container has been shutdown.
		_, err = s.listenerClient.ContainerShutdown(context.Background(), &pb.ContainerShutdownInfo{ContainerName: containerName})
		if err != nil {
			log.Printf("Could not notify ContainerShutdown of %s on host: %v", containerName, err)
		}
	}
	op, err := s.lxd.DeleteContainer(containerName)
	if err != nil {
		return err
	}
	if err = op.Wait(); err != nil {
		return err
	}
	return nil
}

func (s *tremplinServer) importContainer(containerName, importPath string) {
	req := &pb.ContainerImportProgress{
		ContainerName: containerName,
	}

	// The host must be informed of the final outcome, so ensure it's updated
	// on every exit path.
	defer func() {
		if req == nil {
			return
		}
		_, err := s.listenerClient.UpdateImportStatus(context.Background(), req)
		if err != nil {
			log.Printf("Could not update import status on host: %v", err)
			return
		}
	}()

	importFilename := filepath.Join("/mnt/shared", importPath)

	// Validate architecture of image.
	localArchName, errLocal := osarch.ArchitectureGetLocal()
	localArchId, errId := osarch.ArchitectureId(localArchName)
	if errLocal != nil || errId != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to read local architecture: %v, %v", errLocal, errId)
		return
	}
	supportedArchs := []int{localArchId}
	personalities, _ := osarch.ArchitecturePersonalities(localArchId)
	supportedArchs = append(supportedArchs, personalities...)

	importFile, err := os.Open(importFilename)
	if err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to open import file to read metadata.yaml %s: %v", importFilename, err)
		return
	}
	defer importFile.Close()

	// Read metadata.yaml from tarball.  If it is not the first file, then skip this check.
	zipr, err := gzip.NewReader(importFile)
	if err == nil {
		tarr := tar.NewReader(zipr)
		h, err := tarr.Next()
		metadataReadOK := false
		if err == nil && h.Name == "metadata.yaml" {
			buf, _ := ioutil.ReadAll(tarr)
			metadata := api.ImageMetadata{}
			err = yaml.Unmarshal(buf, &metadata)
			if err == nil {
				metadataReadOK = true
				archId, _ := osarch.ArchitectureId(metadata.Architecture)
				archSupported := false
				for _, arch := range supportedArchs {
					if arch == archId {
						archSupported = true
						break
					}
				}
				if !archSupported {
					req.Status = pb.ContainerImportProgress_FAILED_ARCHITECTURE
					req.FailureReason = fmt.Sprintf("Invalid image architecture %s must match local %s", metadata.Architecture, localArchName)
					req.ArchitectureDevice = localArchName
					req.ArchitectureContainer = metadata.Architecture
					log.Printf(req.FailureReason)
					return
				} else {
					log.Printf("Image architecture %s matches local %s", metadata.Architecture, localArchName)
				}
			}
		}
		if !metadataReadOK {
			log.Printf("Could not read metadata.yaml as first file in image, got file %s, error %v", h.Name, err)
		}
	}

	// Import image.  Reset importFile to start.
	offset, err := importFile.Seek(0, 0)
	if offset != 0 || err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to reset import file to upload %s: %v", importFilename, err)
		return
	}
	fi, err := importFile.Stat()
	if err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to stat import file %s: %v", importFilename, err)
		return
	}

	// Use a ProgressReader as a wrapper for importFile.
	createArgs := &lxd.ImageCreateArgs{
		MetaFile: &ioprogress.ProgressReader{
			ReadCloser: importFile,
			Tracker: &ioprogress.ProgressTracker{
				Length: fi.Size(),
				Handler: func(percent int64, speed int64) {
					req.ProgressPercent = uint32(percent)
					req.ProgressSpeed = uint64(speed)
					req.Status = pb.ContainerImportProgress_IMPORTING_UPLOAD
					_, err = s.listenerClient.UpdateImportStatus(context.Background(), req)
					if err != nil {
						log.Printf("Could not update CreateImage upload file status on host: %v", err)
						return
					}
				},
			},
		},
	}
	log.Printf("Uploading image from file %s, size=%d", importFilename, fi.Size())
	op, err := s.lxd.CreateImage(api.ImagesPost{}, createArgs)
	if err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to create image file %s: %v", importFilename, err)
		return
	}
	var fingerprint string
	// An error for wait is only a problem if fingerprint and size are not
	// returned.  If the image already exists, we can continue with import.
	err = op.Wait()
	if f, ok := op.Get().Metadata["fingerprint"]; ok {
		fingerprint = f.(string)
	}
	if fingerprint == "" {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error waiting to create image %s: %v", importFilename, err)
		return
	}

	// Ensure image is deleted when we are complete or on error.
	defer func() {
		op, err = s.lxd.DeleteImage(fingerprint)
		if err != nil {
			req.Status = pb.ContainerImportProgress_FAILED
			req.FailureReason = fmt.Sprintf("error deleting image %s: %v", fingerprint, err)
			return
		}
		if err = op.Wait(); err != nil {
			req.Status = pb.ContainerImportProgress_FAILED
			req.FailureReason = fmt.Sprintf("error waiting to delete image %s: %v", fingerprint, err)
			return
		}
	}()

	// Delete temp 'rootfs-import' if it exists.
	if err = s.deleteContainer(importContainerName); err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error deleting existing container %s: %v", importContainerName, err)
		return
	}

	// Create a new temp 'rootfs-import' container from the image.
	imgInfo := api.Image{
		Fingerprint: fingerprint,
	}
	reqInit := api.ContainersPost{
		Name: importContainerName,
	}
	log.Printf("Creating temp container %s from image", importContainerName)
	opRemote, err := s.lxd.CreateContainerFromImage(s.lxd, imgInfo, reqInit)
	if err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error creating container %s from image %s: %v", importContainerName, fingerprint, err)
		return
	}
	// Track progress for CreateContainerFromImage.
	_, err = opRemote.AddHandler(func(op api.Operation) {
		stage, percent, speed, ok := getProgress(op)
		if !ok {
			return
		}
		req.ProgressPercent = percent
		req.ProgressSpeed = speed
		switch stage {
		case "create_container_from_image_unpack":
			req.Status = pb.ContainerImportProgress_IMPORTING_UNPACK
		default:
			log.Printf("Unknown CreateContainerFromImage stage: %v", stage)
			return
		}
		_, err = s.listenerClient.UpdateImportStatus(context.Background(), req)
		if err != nil {
			log.Printf("Could not update CreateContainerFromImage status on host: %v", err)
			return
		}
	})
	if err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error adding progress handler: %v", err)
		return
	}

	if err = opRemote.Wait(); err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error waiting to create container %s from image %s: %v", importContainerName, fingerprint, err)
		return
	}

	log.Printf("Deleting container %s and replacing from temp container %s", containerName, importContainerName)
	// Delete container <containerName> if it exists.
	if err = s.deleteContainer(containerName); err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error deleting existing container %s: %v", containerName, err)
		return
	}

	// Rename 'rootfs-import' to <containerName>.
	op, err = s.lxd.RenameContainer(importContainerName, api.ContainerPost{Name: containerName})
	if err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error renaming container %s to %s: %v", importContainerName, containerName, err)
		return
	}
	if err = op.Wait(); err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error waiting to rename container %s to %s: %v", importContainerName, containerName, err)
		return
	}

	req.Status = pb.ContainerImportProgress_DONE
	log.Printf("ImportContainer done")
}

// ImportContainer implements tremplin.ImportContainer.
func (s *tremplinServer) ImportContainer(ctx context.Context, in *pb.ImportContainerRequest) (*pb.ImportContainerResponse, error) {
	log.Printf("Received ImportContainer RPC: %s %s", in.ContainerName, in.ImportPath)

	go s.importContainer(in.ContainerName, in.ImportPath)
	response := &pb.ImportContainerResponse{
		Status: pb.ImportContainerResponse_IMPORTING,
	}
	return response, nil
}
