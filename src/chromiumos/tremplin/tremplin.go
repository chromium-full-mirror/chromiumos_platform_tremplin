// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
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
	"time"

	pb "chromiumos/vm_tools/tremplin_proto"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/containerwriter"
	"github.com/lxc/lxd/shared/idmap"
	"github.com/lxc/lxd/shared/ioprogress"
	"github.com/lxc/lxd/shared/osarch"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v2"
)

const (
	backupSnapshot      = "rootfs-backup"
	importContainerName = "rootfs-import"
	suffixRecover       = "-crostini-recover"
	suffixRemap         = "-crostini-remap"
	shiftSnapshot       = "rootfs-shift"

	lingerPath         = "/var/lib/systemd/linger"
	primaryUserID      = 1000
	chronosAccessID    = 1001
	androidRootID      = 655360
	androidEverybodyID = 665357

	exportWriterBufferSize = 16 * 1024 * 1024 // 16Mib.
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

// unmarshalIdmapSets unmarshals the last and next IdmapSets for a Container/ContainerSnapshot.
func unmarshalIdmapSets(name string, config map[string]string) (*idmap.IdmapSet, *idmap.IdmapSet, error) {
	lastIdmap, ok := config["volatile.last_state.idmap"]
	if !ok {
		return nil, nil, fmt.Errorf("no volatile.last_state.idmap key for container %s", name)
	}
	nextIdmap, ok := config["volatile.idmap.next"]
	if !ok {
		return nil, nil, fmt.Errorf("no volatile.idmap.next key for container %s", name)
	}

	// The idmap configs are JSON-encoded arrays of LXD idmap entries.
	var unmarshaledLastIdmap []idmap.IdmapEntry
	if err := json.Unmarshal([]byte(lastIdmap), &unmarshaledLastIdmap); err != nil {
		return nil, nil, err
	}

	var unmarshaledNextIdmap []idmap.IdmapEntry
	if err := json.Unmarshal([]byte(nextIdmap), &unmarshaledNextIdmap); err != nil {
		return nil, nil, err
	}

	lastSet := &idmap.IdmapSet{Idmap: unmarshaledLastIdmap}
	nextSet := &idmap.IdmapSet{Idmap: unmarshaledNextIdmap}
	return lastSet, nextSet, nil
}

// idRemapRequired examines the last and next idmaps for a container and checks
// if the container rootfs will require a remap when it next starts.
func idRemapRequired(c *api.Container) (bool, error) {
	lastSet, nextSet, err := unmarshalIdmapSets(c.Name, c.ExpandedConfig)

	if err != nil {
		return false, err
	}

	// A remap is required only if the last and next IdmapSets don't match.
	return !reflect.DeepEqual(lastSet, nextSet), nil
}

// tremplinServer is used to implement the gRPC tremplin.Server.
type tremplinServer struct {
	lxd                         lxd.ContainerServer
	grpcServer                  *grpc.Server
	listenerClient              pb.TremplinListenerClient
	milestone                   int
	timezoneName                string
	exportImportStatus          TransactionMap
	upgradeStatus               TransactionMap
	upgradeClientUpdateInterval time.Duration
}

// execProgramAsync starts running a program in a container.
func (s *tremplinServer) execProgramAsync(containerName string, args []string, stdout io.WriteCloser, stderr io.WriteCloser) (op lxd.Operation, err error) {
	req := api.ContainerExecPost{
		Command:     args,
		WaitForWS:   true,
		Interactive: false,
	}

	execArgs := &lxd.ContainerExecArgs{
		Stdin:  &stdioSink{},
		Stdout: stdout,
		Stderr: stderr,
	}

	return s.lxd.ExecContainer(containerName, req, execArgs)
}

// execProgram runs a program in a container to completion, capturing its
// return value, stdout, and stderr.
func (s *tremplinServer) execProgram(containerName string, args []string) (ret int, stdout string, stderr string, err error) {
	stdoutSink := &stdioSink{}
	stderrSink := &stdioSink{}

	op, err := s.execProgramAsync(containerName, args, stdoutSink, stderrSink)
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

// deleteSnapshot deletes a snapshot with snapshotName for containerName.
func (s *tremplinServer) deleteSnapshot(containerName, snapshotName string) error {
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

	return nil
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
			if err := s.deleteSnapshot(containerName, snapshotName); err != nil {
				return err
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

// renameContainer renames the specified container from oldName to newName.
func (s *tremplinServer) renameContainer(oldName, newName string) error {
	op, err := s.lxd.RenameContainer(oldName, api.ContainerPost{Name: newName})
	if err != nil {
		return fmt.Errorf("error calling rename container %s to %s: %v", oldName, newName, err)
	}
	if err = op.Wait(); err != nil {
		return fmt.Errorf("error waiting to rename container %s to %s: %v", oldName, newName, err)
	}
	return nil
}

// stopContainer stops the specified container.
func (s *tremplinServer) stopContainer(containerName string) error {
	reqState := api.ContainerStatePut{
		Action:  "stop",
		Timeout: -1,
		Force:   true,
	}
	op, err := s.lxd.UpdateContainerState(containerName, reqState, "")
	if err != nil {
		return fmt.Errorf("error calling stop container %s: %v", containerName, err)
	}
	if err = op.Wait(); err != nil {
		return fmt.Errorf("error waiting to stop container %s: %v", containerName, err)
	}
	return nil
}

// writeContainerFile writes content to fileName in containerName.
func (s *tremplinServer) writeContainerFile(containerName, fileName, content string) error {
	args := lxd.ContainerFileArgs{
		Content:   strings.NewReader(content),
		UID:       0,
		GID:       0,
		Mode:      0644,
		Type:      "file",
		WriteMode: "overwrite",
	}
	err := s.lxd.CreateContainerFile(containerName, fileName, args)
	if err != nil {
		return fmt.Errorf("error writing file %s in container %s: %v", fileName, containerName, err)
	}
	return nil
}

// getContainerWithRecovery resets the container if a previous operation such
// as remap has failed.  Returns valid container, or nil if no container or recovery exists.
func (s *tremplinServer) getContainerWithRecovery(containerName string) (*api.Container, string, error) {
	container, etag, err := s.lxd.GetContainer(containerName)
	if container != nil {
		return container, etag, err
	}

	// If <container> doesn't exist, then restore it from <container>-crostini-recover if it exists.
	recoverName := containerName + suffixRecover
	log.Printf("Container %s not found, checking for recovery container %s", containerName, recoverName)
	container, etag, err = s.lxd.GetContainer(recoverName)
	if container == nil {
		log.Printf("Recovery container %s not found, this must be first setup", recoverName)
		return container, etag, err
	}
	log.Printf("Recovery container found, renaming %s to %s", recoverName, containerName)
	err = s.renameContainer(recoverName, containerName)
	if err != nil {
		return nil, "", err
	}

	// Return updated container.
	log.Printf("Recovery complete for %s", containerName)
	return s.lxd.GetContainer(containerName)
}

// remapContainer copies a container and does remapping on the copy before swapping them over.
// This process combined with using getContainerWithRecovery when first accessing the container
// allows recovery if the remap process is stopped at any point.
func (s *tremplinServer) remapContainer(container *api.Container) (*api.Container, string, error) {
	recoverName := container.Name + suffixRecover
	remapName := container.Name + suffixRemap

	log.Printf("Remap: start id remap for %s", container.Name)
	// Delete old snapshot which was previously created before remap.
	// TODO(crbug.com/995649): Deleting 'rootfs-shift' can be removed after M80.
	// It was previously being created for id remap, but never deleted.
	// Ignore any errors deleting this snapshot.
	log.Printf("Remap: delete old %s/%s snapshot if it exists", container.Name, shiftSnapshot)
	snapshot, _, _ := s.lxd.GetContainerSnapshot(container.Name, shiftSnapshot)
	if snapshot != nil {
		log.Printf("Remap: snapshot %s/%s exists, deleting", container.Name, shiftSnapshot)
		op, err := s.lxd.DeleteContainerSnapshot(container.Name, shiftSnapshot)
		if err == nil {
			op.Wait()
			log.Printf("Remap: snapshot delete result: %v", op.Get())
		}
	} else {
		log.Printf("Remap: snapshot %s/%s does not exist", container.Name, shiftSnapshot)
	}

	// Delete <container>-crostini-remap if it exists.
	log.Printf("Remap: delete old remap container %s if it exists", remapName)
	remapConatiner, _, _ := s.lxd.GetContainer(remapName)
	if remapConatiner != nil {
		log.Printf("Remap: Previous remap container %s exists, deleting", remapName)
		err := s.deleteContainer(remapName)
		if err != nil {
			return nil, "", err
		}
	}

	// Delete <container>-crostini-recover if it exists.
	log.Printf("Remap: delete old recovery container %s if it exists", recoverName)
	recoverConatiner, _, _ := s.lxd.GetContainer(recoverName)
	if recoverConatiner != nil {
		log.Printf("Remap: Previous recovery container %s exists, deleting", recoverName)
		err := s.deleteContainer(recoverName)
		if err != nil {
			return nil, "", err
		}
	}

	// Copy <container> to <container>-crostini-remap.
	log.Printf("Remap: copy %s to %s", container.Name, remapName)
	copyArgs := lxd.ContainerCopyArgs{
		Name:          remapName,
		ContainerOnly: true,
	}
	rop, err := s.lxd.CopyContainer(s.lxd, *container, &copyArgs)
	if err != nil {
		return nil, "", fmt.Errorf("error calling copy container for remap %s: %v", remapName, err)
	}
	if err = rop.Wait(); err != nil {
		return nil, "", fmt.Errorf("error waiting for copy container for remap %s: %v", remapName, err)
	}

	// Start <container>-crostini-remap to do the remapping.
	log.Printf("Remap: start %s to do the remapping", remapName)
	reqState := api.ContainerStatePut{
		Action:  "start",
		Timeout: -1,
	}
	op, err := s.lxd.UpdateContainerState(remapName, reqState, "")
	if err != nil {
		return nil, "", fmt.Errorf("error calling start container for remap %s: %v", remapName, err)
	}
	if err = op.Wait(); err != nil {
		return nil, "", fmt.Errorf("error waiting for start container for remap %s: %v", remapName, err)
	}
	opAPI := op.Get()
	if opAPI.StatusCode != api.Success {
		return nil, "", fmt.Errorf("error waiting starting container for remap %s: %v", remapName, opAPI.StatusCode)
	}

	// Stop <container>-crostini-remap.
	log.Printf("Remap: stop %s", remapName)
	err = s.stopContainer(remapName)
	if err != nil {
		return nil, "", err
	}

	// Update <container>-crostini-remap /etc/hostname to be <container>.
	log.Printf("Remap: update %s /etc/hostname to %s", remapName, container.Name)
	err = s.writeContainerFile(remapName, "/etc/hostname", container.Name)
	if err != nil {
		return nil, "", err
	}

	// Rename <container> to <container>-crostini-recover.
	log.Printf("Remap: rename %s to %s", container.Name, recoverName)
	err = s.renameContainer(container.Name, recoverName)
	if err != nil {
		return nil, "", err
	}

	// Rename <container>-crostini-remap to <container>.
	log.Printf("Remap: rename %s to %s", remapName, container.Name)
	err = s.renameContainer(remapName, container.Name)
	if err != nil {
		return nil, "", err
	}

	// Delete <container>-crostini-recover.
	log.Printf("Remap: delete %s", recoverName)
	err = s.deleteContainer(recoverName)
	if err != nil {
		return nil, "", err
	}

	// Return updated container.
	log.Printf("Remap: complete for %s", container.Name)
	return s.lxd.GetContainer(container.Name)
}

// startContainer performs pre-processing of the container and starts it.
// This function is designed to be run async to allow StartContainer to return quickly.
func (s *tremplinServer) startContainer(container *api.Container, etag string, in *pb.StartContainerRequest, remap bool) {
	req := &pb.ContainerStartProgress{
		ContainerName: container.Name,
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

	// Prepare SSH keys, token, and apt config to bind-mount in.
	// Clear out all existing devices for the container.
	containerPut := container.Writable()
	containerPut.Devices = map[string]map[string]string{}
	err := os.MkdirAll(fmt.Sprintf("/run/sshd/%s", container.Name), 0644)
	if err != nil {
		req.Status = pb.ContainerStartProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to create ssh key dir: %v", err)
		return
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

	for _, b := range bindMounts {
		// Disregard bind mounts without values.
		if b.content == "" {
			continue
		}

		err = ioutil.WriteFile(b.source, []byte(b.content), 0644)
		if err != nil {
			req.Status = pb.ContainerStartProgress_FAILED
			req.FailureReason = fmt.Sprintf("failed to write %q: %v", b.source, err)
			return
		}

		containerPut.Devices[b.name] = map[string]string{
			"source": b.source,
			"path":   b.dest,
			"type":   "disk",
		}
	}

	op, err := s.lxd.UpdateContainer(container.Name, containerPut, etag)
	if err != nil {
		req.Status = pb.ContainerStartProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to set up devices: %v", err)
		return
	}
	if err = op.Wait(); err != nil {
		req.Status = pb.ContainerStartProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to wait for container update: %v", err)
		return
	}
	opAPI := op.Get()
	if opAPI.StatusCode != api.Success {
		req.Status = pb.ContainerStartProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to update container: %v", err)
		return
	}

	if remap {
		container, etag, err = s.remapContainer(container)
		if err != nil {
			log.Printf("Remap: error: %v", err)
			req.Status = pb.ContainerStartProgress_FAILED
			req.FailureReason = fmt.Sprintf("failed to remap: %v", err)
			return
		}
	}

	reqState := api.ContainerStatePut{
		Action:  "start",
		Timeout: -1,
	}
	op, err = s.lxd.UpdateContainerState(container.Name, reqState, "")
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
	opAPI = op.Get()

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

func (s *tremplinServer) handleCreateImageOperation(name string, op api.Operation) {
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
		fingerprint, ok := op.Metadata["fingerprint"].(string)
		if !ok {
			req.Status = pb.ContainerCreationProgress_FAILED
			req.FailureReason = "no fingerprint for imported image"
			break
		}

		containersPost := api.ContainersPost{
			Name: name,
			Source: api.ContainerSource{
				Type:        "image",
				Fingerprint: fingerprint,
			},
		}
		op, err := s.lxd.CreateContainer(containersPost)
		if err != nil {
			req.Status = pb.ContainerCreationProgress_FAILED
			req.FailureReason = fmt.Sprintf("failed to create container from image: %v", err)
			break
		}
		_, err = op.AddHandler(func(op api.Operation) { s.handleCreateOperation(op) })
		if err != nil {
			log.Fatal("Failed to add create operation handler: ", err)
		}
		return
	case api.Running:
		return
	case api.Cancelled, api.Failure:
		req.Status = pb.ContainerCreationProgress_FAILED
		req.FailureReason = op.Err
	default:
		req.Status = pb.ContainerCreationProgress_UNKNOWN
		req.FailureReason = fmt.Sprintf("unhandled create image status: %s", op.Status)
	}

	_, err := s.listenerClient.UpdateCreateStatus(context.Background(), req)
	if err != nil {
		log.Printf("Could not update create status on host: %v", err)
		return
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

	container, _, _ := s.getContainerWithRecovery(in.ContainerName)
	if container != nil {
		response.Status = pb.CreateContainerResponse_EXISTS
		return response, nil
	}

	// Import the image from tarballs.
	if len(in.RootfsPath) > 0 && len(in.MetadataPath) > 0 {
		rootfsReader, err := os.Open(in.RootfsPath)
		if err != nil {
			response.Status = pb.CreateContainerResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to open image rootfs: %v", err)
			return response, nil
		}
		metaReader, err := os.Open(in.MetadataPath)
		if err != nil {
			response.Status = pb.CreateContainerResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to open image metadata: %v", err)
			return response, nil
		}

		// For non-unified container images the client will copy the images to a tempfile
		// before transferring it to the daemon. Since this can take a while, run CreateImage
		// in a separate goroutine and return CREATING to the host immediately.
		go func() {
			op, err := s.lxd.CreateImage(api.ImagesPost{
				ImagePut: api.ImagePut{},
			}, &lxd.ImageCreateArgs{
				MetaFile:        metaReader,
				MetaName:        filepath.Base(in.MetadataPath),
				RootfsFile:      rootfsReader,
				RootfsName:      filepath.Base(in.RootfsPath),
				ProgressHandler: func(progress ioprogress.ProgressData) {},
			})
			if err != nil {
				req := &pb.ContainerCreationProgress{
					ContainerName: in.ContainerName,
					Status:        pb.ContainerCreationProgress_FAILED,
					FailureReason: fmt.Sprintf("failed to import image: %v", err),
				}
				_, err := s.listenerClient.UpdateCreateStatus(context.Background(), req)
				if err != nil {
					log.Printf("Could not update create status on host: %v", err)
					return
				}
			}
			// If the image import was started successfully, register the normal operation handler.
			_, err = op.AddHandler(func(op api.Operation) { s.handleCreateImageOperation(in.ContainerName, op) })
			if err != nil {
				log.Fatal("Failed to add create image operation handler: ", err)
			}
		}()

		response.Status = pb.CreateContainerResponse_CREATING

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

	container, etag, err := s.getContainerWithRecovery(in.ContainerName)
	if err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to find container: %v", err)
		return response, nil
	}

	if container.StatusCode == api.Running {
		response.Status = pb.StartContainerResponse_RUNNING
		return response, nil
	}

	osRelease, err := getGuestOSRelease(s.lxd, container.Name)
	if err == nil {
		response.OsRelease = osRelease.toProto()
		if osRelease.id == "debian" {
			if osRelease.versionID == "9" {
				err = s.writeContainerFile(container.Name, "/etc/apt/sources.list.d/cros.list", createAptSourceList(s.milestone, "stretch"))
			} else if osRelease.versionID == "10" {
				err = s.writeContainerFile(container.Name, "/etc/apt/sources.list.d/cros.list", createAptSourceList(s.milestone, "buster"))
			} else if osRelease.versionID == "" {
				// Unstable and maybe testing, closest to buster. We used to
				// forcibly downgrade them to stretch versions which is why
				// we upgrade them now instead of leaving it alone.
				err = s.writeContainerFile(container.Name, "/etc/apt/sources.list.d/cros.list", createAptSourceList(s.milestone, "buster"))
			}
		} // else unknown distro so do nothing.
		if err != nil {
			log.Print("Failed to update guest cros.list: ", err)
		}
	} else {
		log.Printf("Could not identify container %q guest distro: %v", container.Name, err)
	}

	remapRequired, err := idRemapRequired(container)
	if err != nil {
		response.Status = pb.StartContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to check if id remap required: %v", err)
		return response, nil
	}

	go s.startContainer(container, etag, in, remapRequired)

	if remapRequired {
		response.Status = pb.StartContainerResponse_REMAPPING

	} else {
		response.Status = pb.StartContainerResponse_STARTING
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

	users := []struct {
		name         string
		uid          uint32
		loginEnabled bool
	}{
		{response.Username, primaryUserID, true},
		{"chronos-access", chronosAccessID, false},
		{"android-everybody", androidEverybodyID, false},
		{"android-root", androidRootID, false},
	}

	for _, user := range users {
		if err := pd.EnsureUserExists(user.name, user.uid, user.loginEnabled); err != nil {
			response.Status = pb.SetUpUserResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to create username=%v, uid=%v, loginEnabled=%v: %v", user.name, user.uid, user.loginEnabled, err)
			return response, nil
		}
	}

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

	// Enable loginctl linger for the target user.
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

type writeTracker struct {
	bytesWritten uint64
}

func (c *writeTracker) Write(bytes []byte) (int, error) {
	c.bytesWritten += uint64(len(bytes))
	return len(bytes), nil
}

func (s *tremplinServer) exportContainer(containerName, exportPath string) {
	req := &pb.ContainerExportProgress{
		ContainerName: containerName,
	}
	var exportFile *os.File

	// The host must be informed of the final outcome, so ensure it's updated
	// on every exit path.
	defer func() {
		if exportFile != nil {
			// If the export isn't succesful or has been cancelled,
			// then the exported file is either invalid or unneeded
			// respectively, so it should be deleted.
			// The file cannot be deleted from within tremplin, as
			// it has been shared in, so the client should delete
			// it.
			// Defensively it is truncated here to reduce disk
			// usage ASAP.
			if req.Status == pb.ContainerExportProgress_FAILED || req.Status == pb.ContainerExportProgress_CANCELLED {
				if err := exportFile.Truncate(0); err != nil {
					log.Printf("Failed to truncate %s: %v", exportFile.Name(), err)
				}
				req.BytesExported = 0
			}
			// Close the export file.
			if err := exportFile.Close(); err != nil {
				log.Printf("Error closing export file %s: %v", exportFile.Name(), err)
			}
		}
		_, err := s.listenerClient.UpdateExportStatus(context.Background(), req)
		if err != nil {
			log.Printf("Could not update export status on host: %v", err)
			return
		}
	}()

	// Create a snapshot for export. It is OK if the container is running.
	if err := s.createSnapshot(containerName, backupSnapshot); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to create backup snapshot %s/%s: %v", containerName, backupSnapshot, err)
		return
	}
	defer func() {
		err := s.deleteSnapshot(containerName, backupSnapshot)
		if err != nil {
			log.Printf("Error deleting snapshot %v for container %v", backupSnapshot, containerName)
		}
	}()

	// Get information (IdmapSets) about the snapshot.
	snapshot, _, err := s.lxd.GetContainerSnapshot(containerName, backupSnapshot)
	if err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to get container %s snapshot %s: %v", containerName, backupSnapshot, err)
		return
	}

	idmapSet, _, err := unmarshalIdmapSets(snapshot.Name, snapshot.ExpandedConfig)
	if err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to get container %s snapshot %s id map from snapshot config: %v", containerName, backupSnapshot, err)
		return
	}

	streamingBackupsDir := shared.VarPath("storage-pools", "default", "streamingbackups")
	// Ensure that the rw directory exists.
	if err = ensureDirectoryExists(streamingBackupsDir); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("Error making streamingbackups directory for container %v: %v", containerName, err)
		return
	}
	streamingSnapshotDir := filepath.Join(streamingBackupsDir, containerName)
	// Remove previous snapshot directory for this container if it exists. This shouldn't happen as the btrfs subvolume delete should clean it up.
	if err = ensureDirectoryDoesntExist(streamingSnapshotDir); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("Error removing old streaming snapshot of container %v: %v", containerName, err)
		return
	}

	snapshotDir := shared.VarPath("snapshots", containerName, backupSnapshot)
	// Create a rw snapshot.
	if _, err = execCommand("btrfs", "subvolume", "snapshot", snapshotDir, streamingSnapshotDir); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to create rw btrfs snapshot %s %s: %v", snapshotDir, streamingSnapshotDir, err)
		return
	}
	defer func() {
		_, err = execCommand("btrfs", "subvolume", "delete", streamingSnapshotDir)
		if err != nil {
			log.Printf("Error cleaning subvolume %v for container %v: %v", streamingSnapshotDir, containerName, err)
		}
	}()

	rootfsDir := filepath.Join(streamingSnapshotDir, "rootfs", "")
	backedUpDirs := []string{
		filepath.Join(streamingSnapshotDir, "metadata.yaml"),
		rootfsDir,
		filepath.Join(streamingSnapshotDir, "templates", ""),
	}

	// Unshift snapshots rootfs.
	if err = idmapSet.UnshiftRootfs(rootfsDir, nil); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to unshift container %s snapshot %s: %v", containerName, backupSnapshot, err)
		return
	}

	// Get snapshot size information for progress updating.
	snapshotNumberFiles, snapshotNumberBytes, err := calculateDiskSpaceInfo(backedUpDirs)
	if err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to calculate size for container %s snapshot %s: %v", containerName, backupSnapshot, err)
		return
	}
	req.TotalInputFiles = snapshotNumberFiles
	req.TotalInputBytes = snapshotNumberBytes

	exportFilePath := filepath.Join("/mnt/shared", exportPath)
	exportFile, err = os.Create(exportFilePath)
	if err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to create export file as %s: %v", exportFilePath, err)
		return
	}

	bufferedExportFile := bufio.NewWriterSize(exportFile, exportWriterBufferSize)
	tracker := &writeTracker{}
	gzWriter := gzip.NewWriter(io.MultiWriter(bufferedExportFile, tracker))
	ctw := containerwriter.NewContainerTarWriter(gzWriter, idmapSet)

	lastUpdate := time.Time{}
	cancelled := false
	fileWriter := func(path string, fi os.FileInfo, err error) error {
		if !cancelled && err == nil {
			err = ctw.WriteFile(len(streamingSnapshotDir)+1, path, fi)
			if err != nil {
				return fmt.Errorf("failed to write file %s for container %s to output %s: %v", path, containerName, exportFilePath, err)
			}
			req.InputFilesStreamed += 1
			if fiSize := fi.Size(); fiSize > 0 {
				req.InputBytesStreamed += uint64(fiSize)
			}
			req.BytesExported = tracker.bytesWritten

			if time.Since(lastUpdate).Seconds() >= 1 {
				cancelled = s.exportImportStatus.StatusIs(containerName, PendingCancel)
				if cancelled {
					return nil
				}

				req.Status = pb.ContainerExportProgress_EXPORTING_STREAMING

				_, err = s.listenerClient.UpdateExportStatus(context.Background(), req)
				if err != nil {
					return fmt.Errorf("failed to update export status while writing container %s: %v", containerName, err)
				}
				lastUpdate = time.Now()
			}
		}
		return nil
	}

	if err := visitFiles(backedUpDirs, fileWriter); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to write container %s: %v", containerName, err)
		return
	}

	if err = ctw.Close(); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error closing tar writer for container %v: %v", containerName, err)
		return
	}

	if err = gzWriter.Close(); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("error closing gz writer for container %v: %v", containerName, err)
		return
	}

	if err = bufferedExportFile.Flush(); err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to flush container writer %s to tar: %v", containerName, err)
		return
	}

	if cancelled {
		req.Status = pb.ContainerExportProgress_CANCELLED
		log.Printf("ExportContainer cancelled")
	} else {
		req.Status = pb.ContainerExportProgress_DONE
		log.Printf("ExportContainer done")
	}
}

// ExportContainer implements tremplin.ExportContainer.
func (s *tremplinServer) ExportContainer(ctx context.Context, in *pb.ExportContainerRequest) (*pb.ExportContainerResponse, error) {
	log.Printf("Received ExportContainer RPC: %s %s", in.ContainerName, in.ExportPath)

	if !s.exportImportStatus.StartTransaction(in.ContainerName) {
		// !started is true iff there is a collision on container names in the
		// transaction map, this should never happen as the invariant that only a
		// single operation for a given container can occur at the same time is
		// checked by the client.
		log.Printf("Cannot start transaction as one is already in progress for container: %s", in.ContainerName)
		response := &pb.ExportContainerResponse{
			Status: pb.ExportContainerResponse_FAILED,
		}
		return response, nil
	}

	go func() {
		s.exportContainer(in.ContainerName, in.ExportPath)
		if !s.exportImportStatus.Remove(in.ContainerName) {
			// !removed is true iff no transaction exists for the container name, this
			// should never happen, and if it does no cleanup is required.
			log.Printf("Couldn't remove transaction as it wasn't found for container: %s", in.ContainerName)
		}
	}()
	response := &pb.ExportContainerResponse{
		Status: pb.ExportContainerResponse_EXPORTING,
	}
	return response, nil
}

// CancelExportContainer implements tremplin.CancelExportContainer .
func (s *tremplinServer) CancelExportContainer(ctx context.Context, in *pb.CancelExportContainerRequest) (*pb.CancelExportContainerResponse, error) {
	log.Printf("Received CancelExportContainer RPC: %v", in.InProgressContainerName)

	if s.exportImportStatus.SetStatus(in.InProgressContainerName, PendingCancel) {
		return &pb.CancelExportContainerResponse{Status: pb.CancelExportContainerResponse_CANCEL_QUEUED}, nil
	} else {
		return &pb.CancelExportContainerResponse{Status: pb.CancelExportContainerResponse_OPERATION_NOT_FOUND}, nil
	}
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
		err := s.stopContainer(containerName)
		if err != nil {
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
		return fmt.Errorf("error calling delete container %s: %v", containerName, err)
	}
	if err = op.Wait(); err != nil {
		return fmt.Errorf("error waiting for delete container %s: %v", containerName, err)
	}
	return nil
}

func (s *tremplinServer) importContainer(containerName, importPath string, availableDiskSpaceBytes uint64) {
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

	// Read metadata.yaml from tarball. If it is not the first file, then skip this check.
	if zipr, err := gzip.NewReader(importFile); err == nil {
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

	fi, err := importFile.Stat()
	if err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to stat import file %s: %v", importFilename, err)
		log.Printf(req.FailureReason)
		return
	}

	if availableDiskSpaceBytes != 0 {
		// Read the gzip ISIZE field (uncompressed input size) which is stored as the
		// last 4 bytes (possibly truncated) little-endian.
		if fi.Size() < 4 {
			req.Status = pb.ContainerImportProgress_FAILED
			req.FailureReason = fmt.Sprintf("unexpected file size %v for %s", fi.Size(), importFilename)
			log.Printf(req.FailureReason)
			return
		}
		buf := make([]byte, 4)
		if read, err := importFile.ReadAt(buf, fi.Size()-4); read != 4 || err != nil {
			req.Status = pb.ContainerImportProgress_FAILED
			req.FailureReason = fmt.Sprintf("failed to read isize from %s read %v bytes: %v", importFilename, read, err)
			log.Printf(req.FailureReason)
			return
		}
		compressedSize := uint64(fi.Size())
		isize := uint64(binary.LittleEndian.Uint32(buf))

		// ISIZE is only 32-bits and is not accurate if the uncompressed size
		// is greater than ~4G. Assuming that the uncompressed file must be
		// larger than compressed, we can estimate minUncompressedSize by
		// repeatedly adding 4G to ISIZE until it is larger than compressed.
		// The same outcome can be achieved without introducing loops by oring
		// the top 32 bits of the compressed size with the isize, and rounding
		// up by 1<<32 once if necessary.
		minUncompressedSize := (compressedSize &^ 0xffffffff) | isize
		if minUncompressedSize < compressedSize {
			minUncompressedSize += 1 << 32
		}

		// Lxd copies the compressed file into its storage before uncompressing it.
		minDiskUsage := minUncompressedSize + compressedSize
		if minDiskUsage > availableDiskSpaceBytes {
			req.Status = pb.ContainerImportProgress_FAILED_SPACE
			req.DiskSpaceAvailableBytes = availableDiskSpaceBytes
			req.DiskSpaceRequiredBytes = minDiskUsage
			req.FailureReason = fmt.Sprintf("insufficient space for import, have %v bytes but needed at least %v bytes", availableDiskSpaceBytes, minDiskUsage)
			log.Printf(req.FailureReason)
			return
		}
	}

	// Import image. Reset importFile to start.
	if offset, err := importFile.Seek(0, 0); offset != 0 || err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to reset import file to upload %s: %v", importFilename, err)
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
	// returned. If the image already exists, we can continue with import.
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

	if s.exportImportStatus.StatusIs(containerName, PendingCancel) {
		req.Status = pb.ContainerImportProgress_CANCELLED
		log.Printf("ImportContainer cancelled")
		return
	}

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

	if s.exportImportStatus.StatusIs(containerName, PendingCancel) {
		req.Status = pb.ContainerImportProgress_CANCELLED
		log.Printf("ImportContainer cancelled")
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
	err = s.renameContainer(importContainerName, containerName)
	if err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = err.Error()
		return
	}

	req.Status = pb.ContainerImportProgress_DONE
	log.Printf("ImportContainer done")
}

// ImportContainer implements tremplin.ImportContainer.
func (s *tremplinServer) ImportContainer(ctx context.Context, in *pb.ImportContainerRequest) (*pb.ImportContainerResponse, error) {
	log.Printf("Received ImportContainer RPC: %s %s %v", in.ContainerName, in.ImportPath, in.AvailableDiskSpace)

	if !s.exportImportStatus.StartTransaction(in.ContainerName) {
		// !started is true iff there is a collision on container names in the
		// transaction map, this should never happen as the invariant that only a
		// single operation for a given container can occur at the same time is
		// checked by the client.
		log.Printf("Collision in TransactionMap for container: %s", in.ContainerName)
		response := &pb.ImportContainerResponse{
			Status: pb.ImportContainerResponse_FAILED,
		}
		return response, nil
	}

	go func() {
		s.importContainer(in.ContainerName, in.ImportPath, in.AvailableDiskSpace)
		if !s.exportImportStatus.Remove(in.ContainerName) {
			// !removed is true iff no transaction exists for the container name, this
			// should never happen, and if it does no cleanup is required.
			log.Printf("Couldn't remove transaction as it wasn't found for container: %s", in.ContainerName)
		}
	}()
	response := &pb.ImportContainerResponse{
		Status: pb.ImportContainerResponse_IMPORTING,
	}
	return response, nil
}

// CancelImportContainer implements tremplin.CancelImportContainer.
func (s *tremplinServer) CancelImportContainer(ctx context.Context, in *pb.CancelImportContainerRequest) (*pb.CancelImportContainerResponse, error) {
	log.Printf("Received CancelImportContainer RPC: %v", in.InProgressContainerName)

	if s.exportImportStatus.SetStatus(in.InProgressContainerName, PendingCancel) {
		return &pb.CancelImportContainerResponse{Status: pb.CancelImportContainerResponse_CANCEL_QUEUED}, nil
	} else {
		return &pb.CancelImportContainerResponse{Status: pb.CancelImportContainerResponse_OPERATION_NOT_FOUND}, nil
	}
}

// UpgradeContainer implements tremplin.UpgradeContainer.
func (s *tremplinServer) UpgradeContainer(ctx context.Context, in *pb.UpgradeContainerRequest) (*pb.UpgradeContainerResponse, error) {
	status, msg := s.startUpgradeContainer(in.ContainerName, in.SourceVersion, in.TargetVersion)
	return &pb.UpgradeContainerResponse{
		Status:        status,
		FailureReason: msg,
	}, nil
}

// CancelUpgradeContainer implements (or will) tremplin.CancelUpgradeContainer.
func (s *tremplinServer) CancelUpgradeContainer(ctx context.Context, in *pb.CancelUpgradeContainerRequest) (*pb.CancelUpgradeContainerResponse, error) {
	log.Printf("Not implemented")
	return &pb.CancelUpgradeContainerResponse{Status: pb.CancelUpgradeContainerResponse_UNKNOWN}, nil
}
