// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	pb "chromiumos/vm_tools/tremplin_proto"
	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared/api"
	"google.golang.org/grpc"
)

const shiftSnapshot = "rootfs-shift"

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
func getDownloadPercentage(opMetadata map[string]interface{}) (int32, error) {
	progress, ok := opMetadata["download_progress"].(string)
	if !ok {
		// If there's no progress metadata, the download hasn't started yet.
		return 0, nil
	}

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

// createShiftSnapshot creates a snapshot for containerName in preparation
// for a rootfs id shift.
func (s *tremplinServer) createShiftSnapshot(containerName string) error {
	snapshots, err := s.lxd.GetContainerSnapshots(containerName)
	if err != nil {
		return fmt.Errorf("failed to get container snapshots: %v", err)
	}

	// Delete any existing shift snapshot.
	for _, snapshot := range snapshots {
		if snapshot.Name == shiftSnapshot {
			op, err := s.lxd.DeleteContainerSnapshot(containerName, shiftSnapshot)
			if err != nil {
				return fmt.Errorf("failed to delete existing snapshot: %v", err)
			}
			if err = op.Wait(); err != nil {
				return fmt.Errorf("failed to wait for snapshot deletion: %v", err)
			}
			opAPI := op.Get()
			if opAPI.StatusCode != api.Success {
				return fmt.Errorf("snapshot deletion failed: %s", opAPI.Err)
			}

			break
		}
	}

	op, err := s.lxd.CreateContainerSnapshot(containerName, api.ContainerSnapshotsPost{
		Name:     shiftSnapshot,
		Stateful: false,
	})
	if err != nil {
		return fmt.Errorf("failed to create container snapshot: %v", err)
	}
	if err = op.Wait(); err != nil {
		return fmt.Errorf("failed to wait for snapshot creation: %v", err)
	}
	opAPI := op.Get()
	if opAPI.StatusCode != api.Success {
		return fmt.Errorf("snapshot creation failed: %s", opAPI.Err)
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
		if err := s.createShiftSnapshot(containerName); err != nil {
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
		downloadPercent, err := getDownloadPercentage(op.Metadata)
		if err != nil {
			log.Printf("Failed to parse download percentage: %v", err)
			return
		}
		req.DownloadProgress = downloadPercent
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
				container.Config["environment.TZ"] = ""
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

	c, _, err := s.lxd.GetContainer(in.ContainerName)
	if err != nil {
		response.Status = pb.GetContainerUsernameResponse_CONTAINER_NOT_FOUND
		response.FailureReason = fmt.Sprintf("failed to find container: %v", err)
		return response, nil
	}
	if c.StatusCode != api.Running {
		response.Status = pb.GetContainerUsernameResponse_CONTAINER_NOT_RUNNING
		response.FailureReason = fmt.Sprintf("container not running, status is: %d", c.StatusCode)
		return response, nil
	}

	// Get username.
	ret, stdout, stderr, err := s.execProgram(in.ContainerName, []string{"id", "-nu", "1000"})
	if err != nil {
		response.Status = pb.GetContainerUsernameResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to run id program: %v", err)
		return response, nil
	}
	if ret != 0 {
		response.Status = pb.GetContainerUsernameResponse_USER_NOT_FOUND
		response.FailureReason = fmt.Sprintf("failed to get user for uid: %v", stderr)
		return response, nil
	}
	response.Username = strings.TrimSpace(stdout)

	// Get homedir.  It is section 6 of passwd entry.  E.g.:
	// testuser:x:1000:1000::/home/testuser:/bin/bash
	ret, stdout, stderr, err = s.execProgram(in.ContainerName, []string{"getent", "passwd", response.Username})
	if err != nil {
		response.Status = pb.GetContainerUsernameResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to run getent program: %v", err)
		return response, nil
	}
	if ret != 0 {
		response.Status = pb.GetContainerUsernameResponse_USER_NOT_FOUND
		response.FailureReason = fmt.Sprintf("failed to get passwd entry: %v", stderr)
		return response, nil
	}
	parts := strings.Split(strings.TrimSpace(stdout), ":")
	if len(parts) <= 5 {
		response.Status = pb.GetContainerUsernameResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to parse homedir in passwd entry: %v", stdout)
		return response, nil
	}
	response.Homedir = parts[5]

	response.Status = pb.GetContainerUsernameResponse_SUCCESS
	return response, nil
}

// SetUpUser implements tremplin.SetUpUser.
func (s *tremplinServer) SetUpUser(ctx context.Context, in *pb.SetUpUserRequest) (*pb.SetUpUserResponse, error) {
	log.Printf("Received SetUpUser RPC: %s", in.ContainerName)

	response := &pb.SetUpUserResponse{}
	response.Username = in.ContainerUsername

	// Groups for user 1000.
	var groups []string

	// Create uid 1000 if it doesn't already exist.
	ret, stdout, stderr, err := s.execProgram(in.ContainerName, []string{"id", "-nu", "1000"})
	if err != nil {
		response.Status = pb.SetUpUserResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to check 1000 uid: %v", err)
		return response, nil
	}
	if ret == 0 {
		// User already exists, capture username.
		response.Username = strings.TrimSpace(stdout)
	} else {
		ret, _, stderr, err = s.execProgram(in.ContainerName,
			[]string{"useradd", "-u", "1000", "-s", "/bin/bash", "-m", in.ContainerUsername})
		if err != nil {
			response.Status = pb.SetUpUserResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to run 1000 useradd: %v", err)
			return response, nil
		}
		if ret != 0 {
			response.Status = pb.SetUpUserResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to add 1000 user: %s", stderr)
			return response, nil
		}
		groups = append(groups,
			"audio",
			"cdrom",
			"dialout",
			"floppy",
			"plugdev",
			"sudo",
			"users",
			"video")
	}

	// Add groups, but don't fail - groups might not exist in the container.
	for _, group := range groups {
		ret, _, stderr, _ = s.execProgram(in.ContainerName,
			[]string{"usermod", "-aG", group, response.Username})
	}

	// Enable loginctl linger for the target user.
	ret, _, stderr, err = s.execProgram(in.ContainerName,
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

	containers, err := s.lxd.GetContainers()
	if err != nil {
		return response, err
	}

	for _, container := range containers {
		var failureReasons []string
		// First option, use timedatectl.
		ret, _, _, err := s.execProgram(container.Name, []string{"timedatectl", "set-timezone", in.TimezoneName})
		if err == nil && ret == 0 {
			response.Successes++
			continue
		}
		failureReasons = append(failureReasons, fmt.Sprintf("setting timezone by name failed: (error: %v, return code: %d)", err, ret))

		// Second option, set the TZ environment variable for this particular container.
		if in.PosixTzString == "" {
			failureReasons = append(failureReasons, fmt.Sprintf("setting timezone by TZ variable failed: no POSIX TZ string provided"))
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
			failureReasons = append(failureReasons, fmt.Sprintf("setting timezone by TZ variable failed: %v", err))
		}

		response.FailureReasons = append(response.FailureReasons, fmt.Sprintf("container %s: %s", container.Name, strings.Join(failureReasons, ", ")))
	}

	return response, nil
}
