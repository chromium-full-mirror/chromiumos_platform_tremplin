// Copyright 2018 The ChromiumOS Authors
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
	"golang.org/x/sys/unix"
	"io"
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

	pb "go.chromium.org/chromiumos/vm_tools/tremplin_proto"

	"github.com/elastic/go-libaudit"
	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/idmap"
	"github.com/lxc/lxd/shared/instancewriter"
	"github.com/lxc/lxd/shared/ioprogress"
	"github.com/lxc/lxd/shared/osarch"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v2"
)

const (
	backupSnapshot      = "rootfs-backup"
	importContainerName = "rootfs-import"
	createContainerName = "crostini-inprogress-create"
	suffixRecover       = "-crostini-recover"
	suffixRemap         = "-crostini-remap"
	shiftSnapshot       = "rootfs-shift"

	tremplinCreatedKey = "createdByTremplin"
	setupFinishedKey   = "tremplinSetupFinished"

	lingerPath         = "/var/lib/systemd/linger"
	PrimaryUserID      = 1000
	ChronosAccessID    = 1001
	AndroidRootID      = 655360
	AndroidEverybodyID = 665357
	lxdSubuidStart     = 1000000

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
	// Expected components are: "", "1.0", "containers", "<container name>",
	// or "", "1.0", "instances", "<container name>".
	if len(components) != 4 {
		return "", fmt.Errorf("invalid source path: %q", s)
	}
	if components[2] != "containers" && components[2] != "instances" {
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
		return 0, fmt.Errorf("failed to convert download percent in '%q' to int: %q", progress, matches[2])
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
func idRemapRequired(c *api.Instance) (bool, error) {
	lastSet, nextSet, err := unmarshalIdmapSets(c.Name, c.ExpandedConfig)

	if err != nil {
		return false, err
	}

	// A remap is required only if the last and next IdmapSets don't match.
	// In LXD 3.17, the initial value of lastSet becomes the empty list
	// rather then the initial id map. This does not mean we need to do a
	// remapping, so don't start one.
	return lastSet.Len() != 0 && !reflect.DeepEqual(lastSet, nextSet), nil
}

// Checks if |privilegeLevel| is valid.
func isPrivilegeLevelValid(privilegeLevel pb.StartContainerRequest_PrivilegeLevel) bool {
	switch privilegeLevel {
	case pb.StartContainerRequest_UNCHANGED, pb.StartContainerRequest_UNPRIVILEGED, pb.StartContainerRequest_PRIVILEGED:
		return true
	default:
		return false
	}
}

// tremplinServer is used to implement the gRPC tremplin.Server.
type tremplinServer struct {
	lxd                         lxd.InstanceServer
	subnet                      string
	grpcServer                  *grpc.Server
	listenerClient              pb.TremplinListenerClient
	milestone                   int
	timezoneName                string
	exportImportStatus          TransactionMap
	lxdHelper                   lxdHelper
	features                    Features
	ueventSocket                int
	auditClient                 *libaudit.AuditClient
	usbManager                  *containerUsbManager
}

// register for lifecycle events from lxd
func (s *tremplinServer) subscribeToEvents() error {
	listener, err := s.lxd.GetEvents()
	if err != nil {
		return err
	}

	_, err = listener.AddHandler([]string{"lifecycle"}, func(e api.Event) {
		var unmarshaledLifecycle api.EventLifecycle
		if err := json.Unmarshal(e.Metadata, &unmarshaledLifecycle); err != nil {
			log.Print("Error unmarshalling lifecycle: ", err)
			return
		}
		log.Printf("Received lifecycle event, Action: %s, Source: %s",
			unmarshaledLifecycle.Action, unmarshaledLifecycle.Source)

		switch unmarshaledLifecycle.Action {
		case "instance-shutdown", "instance-stopped":
			containerName, err := getContainerName(unmarshaledLifecycle.Source)
			if err != nil {
				log.Printf("Could not parse container name: %v", err)
				return
			}

			// Notify cicerone that container has been shutdown.
			_, err = s.listenerClient.ContainerShutdown(context.Background(), &pb.ContainerShutdownInfo{ContainerName: containerName})
			if err != nil {
				log.Printf("Could not notify ContainerShutdown of %s on host: %v", containerName, err)
			}
		case "instance-deleted":
			containerName, err := getContainerName(unmarshaledLifecycle.Source)
			if err != nil {
				log.Printf("Could not parse container name: %v", err)
				return
			}

			// Detach USB devices from the container
			m := s.usbManager
			m.Lock()
			for i := 1; i <= len(m.portInfo); i++ {
				port := m.getPortInfo(i)
				if port.attached && port.containerName == containerName {
					log.Printf("USB port %d was attached to deleted container %q", i+1, containerName)
					port.attached = false
					port.containerName = ""
				}
			}
			m.Unlock()
		}
	})

	return err
}

// execProgramAsync starts running a program in a container.
func (s *tremplinServer) execProgramAsync(containerName string, args []string, stdout io.WriteCloser, stderr io.WriteCloser) (op lxd.Operation, err error) {
	req := api.InstanceExecPost{
		Command:     args,
		WaitForWS:   true,
		Interactive: false,
	}

	execArgs := &lxd.InstanceExecArgs{
		Stdin:  &stdioSink{},
		Stdout: stdout,
		Stderr: stderr,
	}

	return s.lxd.ExecInstance(containerName, req, execArgs)
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
	op, err := s.lxd.DeleteInstanceSnapshot(containerName, snapshotName)
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
	names, err := s.lxd.GetInstanceSnapshotNames(containerName)
	if err != nil {
		return fmt.Errorf("failed to get container snapshot names: %v", err)
	}

	// LXD v4 returns a differently formatted name:
	snapshotNameV4 := fmt.Sprintf("/1.0/instances/%s/snapshots/%s", containerName, snapshotName)

	// Delete any existing snapshot with the same name.
	for _, name := range names {
		if name == snapshotName || name == snapshotNameV4 {
			if err := s.deleteSnapshot(containerName, snapshotName); err != nil {
				return err
			}

			break
		}
	}

	op, err := s.lxd.CreateInstanceSnapshot(containerName, api.InstanceSnapshotsPost{
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
	op, err := s.lxd.RenameInstance(oldName, api.InstancePost{Name: newName})
	if err != nil {
		return fmt.Errorf("error calling rename container %s to %s: %v", oldName, newName, err)
	}
	if err = op.Wait(); err != nil {
		return fmt.Errorf("error waiting to rename container %s to %s: %v", oldName, newName, err)
	}
	return nil
}

// stopContainer stops the specified container.
func (s *tremplinServer) stopContainer(containerName string, wait bool) (lxd.Operation, error) {
	reqState := api.InstanceStatePut{
		Action:  "stop",
		Timeout: -1,
		Force:   true,
	}
	log.Printf("stopContainer name: %v lxd: %v", containerName, s.lxd)
	op, err := s.lxd.UpdateInstanceState(containerName, reqState, "")
	if err != nil {
		return nil, fmt.Errorf("error calling stop container %s: %v", containerName, err)
	}
	if !wait {
		return op, nil
	}
	if err = op.Wait(); err != nil {
		return nil, fmt.Errorf("error waiting to stop container %s: %v", containerName, err)
	}
	return nil, nil
}

// writeInstanceFile writes content to fileName in containerName.
func (s *tremplinServer) writeInstanceFile(cfs InstanceFileServer, fileName, content string) error {
	args := lxd.InstanceFileArgs{
		Content:   strings.NewReader(content),
		UID:       0,
		GID:       0,
		Mode:      0644,
		Type:      "file",
		WriteMode: "overwrite",
	}
	err := cfs.CreateInstanceFile(fileName, args)
	if err != nil {
		return fmt.Errorf("error writing file %s: %v", fileName, err)
	}
	return nil
}

// getContainerWithRecovery resets the container if a previous operation such
// as remap has failed.  Returns valid container, or nil if no container or recovery exists.
func (s *tremplinServer) getContainerWithRecovery(containerName string) (*api.Instance, string, error) {
	container, etag, err := s.lxd.GetInstance(containerName)
	if container != nil {
		return container, etag, err
	}

	// If <container> doesn't exist, then restore it from <container>-crostini-recover if it exists.
	recoverName := containerName + suffixRecover
	log.Printf("Container %s not found, checking for recovery container %s", containerName, recoverName)
	container, etag, err = s.lxd.GetInstance(recoverName)
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
	return s.lxd.GetInstance(containerName)
}

// remapContainer copies a container and does remapping on the copy before swapping them over.
// This process combined with using getContainerWithRecovery when first accessing the container
// allows recovery if the remap process is stopped at any point.
func (s *tremplinServer) remapContainer(container *api.Instance) (*api.Instance, string, error) {
	recoverName := container.Name + suffixRecover
	remapName := container.Name + suffixRemap

	log.Printf("Remap: start id remap for %s", container.Name)
	// Delete old snapshot which was previously created before remap.
	// TODO(crbug.com/995649): Deleting 'rootfs-shift' can be removed after M80.
	// It was previously being created for id remap, but never deleted.
	// Ignore any errors deleting this snapshot.
	log.Printf("Remap: delete old %s/%s snapshot if it exists", container.Name, shiftSnapshot)
	snapshot, _, _ := s.lxd.GetInstanceSnapshot(container.Name, shiftSnapshot)
	if snapshot != nil {
		log.Printf("Remap: snapshot %s/%s exists, deleting", container.Name, shiftSnapshot)
		op, err := s.lxd.DeleteInstanceSnapshot(container.Name, shiftSnapshot)
		if err == nil {
			op.Wait()
			log.Printf("Remap: snapshot delete result: %v", op.Get())
		}
	} else {
		log.Printf("Remap: snapshot %s/%s does not exist", container.Name, shiftSnapshot)
	}

	// Delete <container>-crostini-remap if it exists.
	log.Printf("Remap: delete old remap container %s if it exists", remapName)
	remapContainer, _, _ := s.lxd.GetInstance(remapName)
	if remapContainer != nil {
		log.Printf("Remap: Previous remap container %s exists, deleting", remapName)
		err := s.deleteContainer(remapName)
		if err != nil {
			return nil, "", err
		}
	}

	// Delete <container>-crostini-recover if it exists.
	log.Printf("Remap: delete old recovery container %s if it exists", recoverName)
	recoverContainer, _, _ := s.lxd.GetInstance(recoverName)
	if recoverContainer != nil {
		log.Printf("Remap: Previous recovery container %s exists, deleting", recoverName)
		err := s.deleteContainer(recoverName)
		if err != nil {
			return nil, "", err
		}
	}

	// Copy <container> to <container>-crostini-remap.
	log.Printf("Remap: copy %s to %s", container.Name, remapName)
	copyArgs := lxd.InstanceCopyArgs{
		Name:         remapName,
		InstanceOnly: true,
	}
	rop, err := s.lxd.CopyInstance(s.lxd, *container, &copyArgs)
	if err != nil {
		return nil, "", fmt.Errorf("error calling copy container for remap %s: %v", remapName, err)
	}
	if err = rop.Wait(); err != nil {
		return nil, "", fmt.Errorf("error waiting for copy container for remap %s: %v", remapName, err)
	}

	// Start <container>-crostini-remap to do the remapping.
	log.Printf("Remap: start %s to do the remapping", remapName)
	reqState := api.InstanceStatePut{
		Action:  "start",
		Timeout: -1,
	}
	op, err := s.lxd.UpdateInstanceState(remapName, reqState, "")
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
	_, err = s.stopContainer(remapName, true)
	if err != nil {
		return nil, "", err
	}

	// Update <container>-crostini-remap /etc/hostname to be <container>.
	log.Printf("Remap: update %s /etc/hostname to %s", remapName, container.Name)
	cfs := NewInstanceFileServer(container.Name)
	err = s.writeInstanceFile(cfs, "/etc/hostname", container.Name)
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
	return s.lxd.GetInstance(container.Name)
}

func getBindMounts(containerName string, token string) []bindMount {
	return []bindMount{
		{
			name:    "container_token",
			content: token,
			source:  fmt.Sprintf("/run/tokens/%s_token", containerName),
			dest:    "/dev/.container_token",
		},
	}
}

// startContainer performs pre-processing of the container and starts it.
// This function is designed to be run async to allow StartContainer to return quickly.
func (s *tremplinServer) startContainer(container *api.Instance, etag string, in *pb.StartContainerRequest, remap bool) {
	req := &pb.ContainerStartProgress{
		ContainerName: container.Name,
		Status:        pb.ContainerStartProgress_STARTING,
	}

	// The host must be informed of the final outcome, so ensure it's updated
	// on every exit path.
	done := make(chan error)
	defer func() {
		done <- nil
		if req == nil {
			return
		}
		_, err := s.listenerClient.UpdateStartStatus(context.Background(), req)
		if err != nil {
			log.Printf("Could not update start status on host: %v", err)
			return
		}
	}()

	// Send heartbeats every so often so Chrome knows we're still alive.
	// Our heartbeat and finish message could race but that's fine, Chrome
	// needs to handle that case anyway (it ignores messages it's not
	// ready for).
	ticker := time.NewTicker(time.Second * 30)
	go func() {
		for {
			select {
			case <-ticker.C:
				if req != nil {
					s.listenerClient.UpdateStartStatus(context.Background(), req)
				}
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()

	// Prepare SSH keys, token, and apt config to bind-mount in.
	// Clear out all existing devices for the container.
	containerPut := container.Writable()
	containerPut.Devices = map[string]map[string]string{}
	bindMounts := getBindMounts(container.Name, in.Token)
	for _, b := range bindMounts {
		// Disregard bind mounts without values.
		if b.content == "" {
			continue
		}

		err := os.WriteFile(b.source, []byte(b.content), 0644)
		if err != nil {
			req.Status = pb.ContainerStartProgress_FAILED
			req.FailureReason = fmt.Sprintf("failed to write %q: %v", b.source, err)
			return
		}

		containerPut.Devices[b.name] = map[string]string{
			"source":   b.source,
			"path":     b.dest,
			"type":     "disk",
			"required": "false",
		}
	}

	if !in.DisableAudioCapture {
		// find any pcm capture devices in the VM and create them in the container
		addFilteredDevices(containerPut.Devices, func(path string) bool {
			return micRegex.MatchString(path)
		})
	}

	privilegeLevel := in.PrivilegeLevel
	if !isPrivilegeLevelValid(privilegeLevel) {
		req.Status = pb.ContainerStartProgress_FAILED
		req.FailureReason = fmt.Sprintf("bad privilege level value: %d", privilegeLevel)
		return
	}

	if privilegeLevel != pb.StartContainerRequest_UNCHANGED {
		containerPut.Config["security.privileged"] = strconv.FormatBool(privilegeLevel == pb.StartContainerRequest_PRIVILEGED)
	}

	op, err := s.lxd.UpdateInstance(container.Name, containerPut, etag)
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
		container, _, err = s.remapContainer(container)
		if err != nil {
			log.Printf("Remap: error: %v", err)
			req.Status = pb.ContainerStartProgress_FAILED
			req.FailureReason = fmt.Sprintf("failed to remap: %v", err)
			return
		}
	}

	reqState := api.InstanceStatePut{
		Action:  "start",
		Timeout: -1,
	}
	op, err = s.lxd.UpdateInstanceState(container.Name, reqState, "")
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
		if err := s.setContainerMetadataKey(container.Name, setupFinishedKey); err != nil {
			req.Status = pb.ContainerStartProgress_FAILED
			req.FailureReason = fmt.Sprintf("Error marking setup finished on container: %v", err)
		} else {
			req.Status = pb.ContainerStartProgress_STARTED
			go s.updateAptKeys(container.Name)
		}
	case api.Cancelled:
		req.Status = pb.ContainerStartProgress_CANCELLED
	case api.Failure:
		req.Status = pb.ContainerStartProgress_FAILED
		req.FailureReason = opAPI.Err
	}
}

// updateAptKeys will try to pull updates to the parent Google Linux package signing key,
// including importing any new subkeys. If it fails (e.g. offline) then it logs an error
// and exits.
// NOTE: This function sleeps for 1 minute first, run it async.
func (s *tremplinServer) updateAptKeys(containerName string) {
	time.Sleep(1 * time.Minute) // Wait 1 minute for the container to finish booting.
	ret, stdout, stderr, err := s.execProgram(
		// Update the parent key which pulls in new subkeys.
		// The key fingerprint can be verified at https://www.google.com/linuxrepositories/
		containerName, []string{"apt-key", "adv", "--refresh-keys", "--keyserver", "keyserver.ubuntu.com",
			"EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796"})

	if ret != 0 || err != nil {
		log.Printf("Unable to update keys. Return code: %v, error: %v, stdout: %v, stderr: %v",
			ret, err, stdout, stderr)
	}
}

func (s *tremplinServer) handleCreateImageOperation(name string, op api.Operation) {
	log.Printf("Create image operation handler triggered. Status: %v", op.Status)
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

		containersPost := api.InstancesPost{
			Name: createContainerName,
			Source: api.InstanceSource{
				Type:        "image",
				Fingerprint: fingerprint,
			},
		}
		op, err := s.lxd.CreateInstance(containersPost)
		if err != nil {
			req.Status = pb.ContainerCreationProgress_FAILED
			req.FailureReason = fmt.Sprintf("failed to create container from image: %v", err)
			break
		}
		_, err = op.AddHandler(func(op api.Operation) { s.handleCreateOperation(op, name) })
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

// setContainerMetadataKey sets the given key on the requested container
// (i.e. isContainerMetadataKeySet will return true for it)
func (s *tremplinServer) setContainerMetadataKey(containerName string, key string) error {
	metadata, etag, err := s.lxd.GetInstanceMetadata(containerName)
	if err != nil {
		return err
	}
	metadata.Properties[key] = "true"
	return s.lxd.UpdateInstanceMetadata(containerName, *metadata, etag)
}

// isContainerMetadataKeySet returns true/false if a given key is set/not set
// on the specified container.
func (s *tremplinServer) isContainerMetadataKeySet(containerName string, key string) (bool, error) {
	metadata, _, err := s.lxd.GetInstanceMetadata(containerName)
	if err != nil {
		return false, err
	}
	return metadata.Properties[key] == "true", nil
}

func (s *tremplinServer) handleCreateOperation(op api.Operation, finalName string) {
	containers := op.Resources["containers"]
	log.Printf("Create operation handler triggered. Status: %v", op.Status)

	if len(containers) != 1 {
		log.Printf("Got %v containers instead of 1", len(containers))
		return
	}

	req := &pb.ContainerCreationProgress{
		ContainerName: finalName,
	}

	switch op.StatusCode {
	case api.Pending:
		// The operation will only be here a short time before transitioning to
		// Running. Don't bother informing the host since there's not anything
		// it can do yet.
		return
	case api.Success:
		if err := s.setContainerMetadataKey(createContainerName, tremplinCreatedKey); err != nil {
			req.Status = pb.ContainerCreationProgress_FAILED
			req.FailureReason = fmt.Sprintf("Error marking container as being a Tremplin container: %v", err)
		} else if err = s.renameContainer(createContainerName, finalName); err != nil {
			req.Status = pb.ContainerCreationProgress_FAILED
			req.FailureReason = fmt.Sprintf("Error renaming container to final name: %v", err)
		} else {
			req.Status = pb.ContainerCreationProgress_CREATED
		}
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

	log.Printf("Updating host with creation status: %v", req.Status)
	_, err := s.listenerClient.UpdateCreateStatus(context.Background(), req)
	if err != nil {
		log.Printf("Could not update create status on host: %v", err)
		return
	}
}

func (s *tremplinServer) isContainerSetUp(containerName string) (bool, error) {
	createdByTremplin, err := s.isContainerMetadataKeySet(containerName, tremplinCreatedKey)
	if err != nil {
		return false, err
	}
	if !createdByTremplin {
		// If we weren't created by tremplin (or were created before we started
		// recording this) assume it's set up correctly.
		return true, nil
	}
	setupComplete, err := s.isContainerMetadataKeySet(containerName, setupFinishedKey)
	if err != nil {
		return false, err
	}
	return setupComplete, nil
}

// CreateContainer implements tremplin.CreateContainer.
func (s *tremplinServer) CreateContainer(ctx context.Context, in *pb.CreateContainerRequest) (*pb.CreateContainerResponse, error) {
	log.Printf("Received CreateContainer RPC: %s", in.ContainerName)

	response := &pb.CreateContainerResponse{}

	container, _, _ := s.getContainerWithRecovery(in.ContainerName)
	if container != nil {
		ok, err := s.isContainerSetUp(in.ContainerName)
		if err == nil && !ok {
			log.Printf("Found incomplete tremplin-managed container. Deleting and creating a new container from scratch.")
			s.deleteContainer(in.ContainerName)
		} else {
			if err != nil {
				log.Printf("Error: unable to check metadata for an existing container: %v. Skipped setup complete check.", err)
			}
			response.Status = pb.CreateContainerResponse_EXISTS
			return response, nil
		}
	}

	sendCreationProgress := func(req *pb.ContainerCreationProgress) {
		_, err := s.listenerClient.UpdateCreateStatus(context.Background(), req)
		if err != nil {
			log.Printf("Could not update create status on host: %v", err)
		}
	}

	// If a previous CreateContainer was interrupted delete the incomplete container and start fresh.
	s.deleteContainer(createContainerName)

	// Import the image from tarballs.
	if len(in.RootfsPath) > 0 && len(in.MetadataPath) > 0 {
		log.Printf("Importing from tarball")
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
			log.Printf("Calling lxd.CreateImage for tarball")
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
				sendCreationProgress(req)
				return
			}
			// If the image import was started successfully, register the normal
			// operation handler.
			log.Printf("Registering create image operation handler for tarball")
			_, err = op.AddHandler(func(op api.Operation) { s.handleCreateImageOperation(in.ContainerName, op) })
			if err != nil {
				log.Fatal("Failed to add create image operation handler: ", err)
			}
		}()
	} else {
		// On a slow network, connecting to a server and downloading alias data and image metadata can
		// take a long time. We do this asynchronously.
		go func() {
			req := &pb.ContainerCreationProgress{
				ContainerName: in.ContainerName,
				Status:        pb.ContainerCreationProgress_UNKNOWN,
			}
			defer func() {
				if req.Status != pb.ContainerCreationProgress_UNKNOWN {
					sendCreationProgress(req)
				}
			}()

			log.Printf("Using image server")
			imageServerUrl := strings.Replace(in.ImageServer, "%d", strconv.Itoa(s.milestone), 1)

			imageServer, err := lxd.ConnectSimpleStreams(imageServerUrl, nil)
			if err != nil {
				req.Status = pb.ContainerCreationProgress_FAILED
				req.FailureReason = fmt.Sprintf("failed to connect to simplestreams image server: %v", err)
				return
			}

			alias, _, err := imageServer.GetImageAlias(in.ImageAlias)
			if err != nil {
				req.Status = pb.ContainerCreationProgress_FAILED
				req.FailureReason = fmt.Sprintf("failed to get alias: %v", err)
				return
			}

			log.Printf("Getting image")
			image, _, err := imageServer.GetImage(alias.Target)
			if err != nil {
				req.Status = pb.ContainerCreationProgress_FAILED
				req.FailureReason = fmt.Sprintf("failed to get image for alias: %v", err)
				return
			}

			containersPost := api.InstancesPost{
				Name: createContainerName,
				Source: api.InstanceSource{
					Type:  "image",
					Alias: alias.Name,
				},
			}
			log.Printf("Creating container from image")
			op, err := s.lxd.CreateInstanceFromImage(imageServer, *image, containersPost)
			if err != nil {
				req.Status = pb.ContainerCreationProgress_FAILED
				req.FailureReason = fmt.Sprintf("failed to create container from image: %v", err)
				return
			}

			_, err = op.AddHandler(func(op api.Operation) { s.handleCreateOperation(op, in.ContainerName) })
			if err != nil {
				req.Status = pb.ContainerCreationProgress_FAILED
				req.FailureReason = fmt.Sprintf("failed to add create operation handler: %v", err)
				log.Fatal("Failed to add create operation handler: ", err)
			}
		}()
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

	container, _, err := s.lxd.GetInstance(in.ContainerName)
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
		op, err := s.stopContainer(container.Name, false)

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

// StopContainer implements tremplin.StopContainer.
func (s *tremplinServer) StopContainer(ctx context.Context, in *pb.StopContainerRequest) (*pb.StopContainerResponse, error) {
	log.Printf("Received StopContainer RPC: %s", in.ContainerName)

	response := &pb.StopContainerResponse{}

	container, _, err := s.lxd.GetInstance(in.ContainerName)
	if container == nil {
		response.Status = pb.StopContainerResponse_DOES_NOT_EXIST
		return response, nil
	}
	if err != nil {
		response.Status = pb.StopContainerResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to find container: %v", err)
		return response, nil
	}

	sendStatus := func(op api.Operation) {
		req := &pb.ContainerStopProgress{ContainerName: container.Name}
		switch op.StatusCode {
		case api.Pending, api.Running:
			req.Status = pb.ContainerStopProgress_STOPPING
		case api.Success:
			req.Status = pb.ContainerStopProgress_STOPPED
		case api.Cancelled:
			req.Status = pb.ContainerStopProgress_CANCELLED
			req.FailureReason = op.Err
		case api.Failure:
			req.Status = pb.ContainerStopProgress_FAILED
			req.FailureReason = op.Err
		default:
			req.Status = pb.ContainerStopProgress_UNKNOWN
			req.FailureReason = fmt.Sprintf("unhandled stop status: %s, %s", op.Status, op.Err)
		}
		if _, err := s.listenerClient.UpdateStopStatus(context.Background(), req); err != nil {
			log.Printf("Could not update stop status on host: %v", err)
		}

	}

	if container.StatusCode == api.Running {
		op, err := s.stopContainer(container.Name, false)

		if err != nil {
			response.Status = pb.StopContainerResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to stop container: %v", err)
			return response, nil
		}

		_, err = op.AddHandler(sendStatus)

		if err != nil {
			response.Status = pb.StopContainerResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to add stop operation handler: %v", err)
			return response, nil
		}
		response.Status = pb.StopContainerResponse_STOPPING
	} else {
		response.Status = pb.StopContainerResponse_STOPPED
	}

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
	op, err := s.lxd.DeleteInstance(containerName)

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

	// If the container is already running check if the privilege level needs to be updated. If the update failed then treat it as an error.
	// Changing privileges, especially going from privileged to non-privileged, shouldn't silently fail as the user may believe in the wrong
	// security model.
	if container.StatusCode == api.Running {
		response.Status = pb.StartContainerResponse_FAILED
		privilegeLevel := in.PrivilegeLevel
		if !isPrivilegeLevelValid(privilegeLevel) {
			response.FailureReason = fmt.Sprintf("bad privilege level value: %d", privilegeLevel)
			return response, nil
		}

		if privilegeLevel != pb.StartContainerRequest_UNCHANGED {
			containerPut := container.Writable()
			containerPut.Config["security.privileged"] = strconv.FormatBool(privilegeLevel == pb.StartContainerRequest_PRIVILEGED)
			op, err := s.lxd.UpdateInstance(container.Name, containerPut, etag)
			if err != nil {
				response.FailureReason = fmt.Sprintf("failed to update privilege level: %v", err)
				return response, nil
			}
			if err = op.Wait(); err != nil {
				response.FailureReason = fmt.Sprintf("failed to update privilege level: %v", err)
				return response, nil
			}

			opAPI := op.Get()
			if opAPI.StatusCode != api.Success {
				response.FailureReason = fmt.Sprintf("failed to update privilege level: %v", err)
				return response, nil
			}
		}
		response.Status = pb.StartContainerResponse_RUNNING
		return response, nil
	}

	cfs := NewInstanceFileServer(in.ContainerName)
	osRelease, err := getGuestOSRelease(cfs, container.Name)
	if err == nil {
		response.OsRelease = osRelease.toProto()
		if osRelease.id == "debian" {
			err = s.writeInstanceFile(cfs, "/etc/apt/sources.list.d/cros.list", createAptSourceList(s.milestone, osRelease.versionID, osRelease.versionCodename))
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

	_, _, err := s.lxd.GetInstance(in.ContainerName)
	if err != nil {
		response.Status = pb.GetContainerUsernameResponse_CONTAINER_NOT_FOUND
		response.FailureReason = fmt.Sprintf("failed to find container: %v", err)
		return response, nil
	}

	cfs := NewInstanceFileServer(in.ContainerName)
	pd, err := NewPasswdDatabase(cfs)
	if err != nil {
		response.Status = pb.GetContainerUsernameResponse_FAILED
		response.FailureReason = fmt.Sprintf("failed to get container passwd db: %v", err)
		return response, nil
	}

	p := pd.PasswdForUid(PrimaryUserID)
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

	cfs := NewInstanceFileServer(in.ContainerName)
	pd, err := NewPasswdDatabase(cfs)
	if err != nil {
		response.Status = pb.SetUpUserResponse_UNKNOWN
		response.FailureReason = fmt.Sprintf("failed to get container passwd db: %v", err)
		return response, nil
	}

	p := pd.PasswdForUid(PrimaryUserID)
	if p != nil {
		response.Username = p.Name
	} else {
		response.Username = in.ContainerUsername
	}

	// Note: If you add/remove users you probably also need to update raw.idmap
	// in start_lxd.go and the id maps in container_file_server.go.
	users := []struct {
		name         string
		uid          uint32
		loginEnabled bool
	}{
		{response.Username, PrimaryUserID, true},
		{"chronos-access", ChronosAccessID, false},
		{"android-everybody", AndroidEverybodyID, false},
		{"android-root", AndroidRootID, false},
	}

	for _, user := range users {
		if err := pd.EnsureUserExists(user.name, user.uid, user.loginEnabled); err != nil {
			response.Status = pb.SetUpUserResponse_FAILED
			failureReason := fmt.Sprintf("failed to create user, uid=%v, loginEnabled=%v: %v", user.uid, user.loginEnabled, err)
			response.FailureReason = strings.ReplaceAll(failureReason, user.name, "$USERNAME")
			return response, nil
		}
		pd.EnsureGroupExists(user.name, user.uid)
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
		{"kvm", false},
		{"libvirt", false},
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
	if err := cfs.CreateInstanceFile(lingerPath, lxd.InstanceFileArgs{
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
	if err := cfs.CreateInstanceFile(userLingerPath, lxd.InstanceFileArgs{
		Content: nil,
		UID:     0,
		GID:     0,
		Mode:    0644,
		Type:    "file",
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

	c, _, err := s.lxd.GetInstanceState(in.ContainerName)
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
		log.Printf("Failed to find ipv4 address for container %q", in.ContainerName)
	}

	response.Status = pb.GetContainerInfoResponse_RUNNING
	return response, nil
}

// SetTimezone implements tremplin.SetTimezone.
func (s *tremplinServer) SetTimezone(ctx context.Context, in *pb.SetTimezoneRequest) (*pb.SetTimezoneResponse, error) {
	response := &pb.SetTimezoneResponse{}
	log.Printf("Received SetTimezone RPC: %s", in.TimezoneName)
	if s.lxd == nil {
		// LXD hasn't started yet, so nothing to do here
		log.Print("Ignoring SetTimezone request before LXD is running")
		response.FailureReasons = append(response.FailureReasons, "Ignoring SetTimezone request before LXD is running")
		return response, nil
	}

	s.timezoneName = in.TimezoneName
	for _, name := range in.ContainerNames {
		container, _, err := s.lxd.GetInstance(name)
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
			s.lxd.UpdateInstance(container.Name, container.Writable(), "")
			continue
		}
		errors = append(errors, fmt.Sprintf("setting timezone by name failed: (error: %v, return code: %d)", err, ret))

		// Second option, set the TZ environment variable for this particular container.
		if in.PosixTzString == "" {
			errors = append(errors, "setting timezone by TZ variable failed: no POSIX TZ string provided")
		} else {
			container.Config["environment.TZ"] = in.PosixTzString
			operation, err := s.lxd.UpdateInstance(container.Name, container.Writable(), "")
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
	snapshot, _, err := s.lxd.GetInstanceSnapshot(containerName, backupSnapshot)
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

	var stat unix.Stat_t
	err = unix.Stat(rootfsDir, &stat)
	if err != nil {
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to stat container %s root directory: %v", containerName, err)
		return
	}

	if stat.Uid >= lxdSubuidStart {
		// Unshift the rootfs.
		if err = idmapSet.UnshiftRootfs(rootfsDir, nil); err != nil {
			req.Status = pb.ContainerExportProgress_FAILED
			req.FailureReason = fmt.Sprintf("failed to unshift container %s snapshot %s: %v", containerName, backupSnapshot, err)
			return
		}
	} else if stat.Uid == PrimaryUserID || stat.Uid == ChronosAccessID || stat.Uid == AndroidRootID || stat.Uid == AndroidEverybodyID {
		// Unable to determine whether rootfs needs to be unshifted.
		req.Status = pb.ContainerExportProgress_FAILED
		req.FailureReason = fmt.Sprintf("incorrect owner of container %s root directory: %v", containerName, stat.Uid)
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
	ctw := instancewriter.NewInstanceTarWriter(gzWriter, nil)

	lastUpdate := time.Time{}
	cancelled := false
	snapshotPathPrefix := streamingSnapshotDir + "/"
	fileWriter := func(path string, fi os.FileInfo, err error) error {
		if !cancelled && err == nil {
			pathInTarFile := strings.TrimPrefix(path, snapshotPathPrefix)
			err = ctw.WriteFile(pathInTarFile, path, fi, false)
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
	c, _, _ := s.lxd.GetInstance(containerName)
	if c == nil {
		log.Printf("Ignoring request to delete non-existent container %s", containerName)
		return nil
	}
	if c.StatusCode != 0 && c.StatusCode != api.Stopped {
		log.Printf("Force stopping container %s before deleting", containerName)
		_, err := s.stopContainer(containerName, true)
		if err != nil {
			return err
		}
		// Notify cicerone that container has been shutdown.
		_, err = s.listenerClient.ContainerShutdown(context.Background(), &pb.ContainerShutdownInfo{ContainerName: containerName})
		if err != nil {
			log.Printf("Could not notify ContainerShutdown of %s on host: %v", containerName, err)
		}
	}
	op, err := s.lxd.DeleteInstance(containerName)
	if err != nil {
		return fmt.Errorf("error calling delete container %s: %v", containerName, err)
	}
	if err = op.Wait(); err != nil {
		return fmt.Errorf("error waiting for delete container %s: %v", containerName, err)
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

	// Read metadata.yaml from tarball. If it is not the first file, then skip this check.
	if zipr, err := gzip.NewReader(importFile); err == nil {
		tarr := tar.NewReader(zipr)
		h, err := tarr.Next()
		metadataReadOK := false
		if err == nil && h.Name == "metadata.yaml" {
			buf, _ := io.ReadAll(tarr)
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
					log.Print(req.FailureReason)
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
		log.Print(req.FailureReason)
		return
	}

	availableDiskSpaceBytes, err := getStatefulFreeSpace()
	if err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to get stateful partition space: %v", err)
		log.Print(req.FailureReason)
		return
	}

	// Read the gzip ISIZE field (uncompressed input size) which is stored as the
	// last 4 bytes (possibly truncated) little-endian.
	if fi.Size() < 4 {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("unexpected file size %v for %s", fi.Size(), importFilename)
		log.Print(req.FailureReason)
		return
	}
	buf := make([]byte, 4)
	if read, err := importFile.ReadAt(buf, fi.Size()-4); read != 4 || err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to read isize from %s read %v bytes: %v", importFilename, read, err)
		log.Print(req.FailureReason)
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
		log.Print(req.FailureReason)
		return
	}

	// Import image. Reset importFile to start.
	if offset, err := importFile.Seek(0, 0); offset != 0 || err != nil {
		req.Status = pb.ContainerImportProgress_FAILED
		req.FailureReason = fmt.Sprintf("failed to reset import file to upload %s: %v", importFilename, err)
		return
	}

	// Use a ProgressReader as a wrapper for importFile.
	createArgs := &lxd.ImageCreateArgs{
		MetaFile: importFile,
		MetaName: filepath.Base(importFilename),
		ProgressHandler: func(progress ioprogress.ProgressData) {
			req.ProgressPercent = uint32(progress.Percentage)
			req.ProgressSpeed = uint64(0)
			req.Status = pb.ContainerImportProgress_IMPORTING_UPLOAD
			_, err = s.listenerClient.UpdateImportStatus(context.Background(), req)
			if err != nil {
				log.Printf("Could not update CreateImage upload file status on host: %v", err)
				return
			}
		},
		Type: "container",
	}
	log.Printf("Uploading image from file %s, size=%d", importFilename, fi.Size())
	op, err := s.lxd.CreateImage(api.ImagesPost{Filename: createArgs.MetaName}, createArgs)
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
	reqInit := api.InstancesPost{
		Name: importContainerName,
	}
	log.Printf("Creating temp container %s from image", importContainerName)
	opRemote, err := s.lxd.CreateInstanceFromImage(s.lxd, imgInfo, reqInit)
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
		case "create_container_from_image_unpack", "create_instance_from_image_unpack":
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
	log.Printf("Received ImportContainer RPC: %s %s", in.ContainerName, in.ImportPath)

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
		s.importContainer(in.ContainerName, in.ImportPath)
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


// HostNetworkChanged implements tremplin.HostNetworkChanged.
func (s *tremplinServer) HostNetworkChanged(ctx context.Context, in *pb.HostNetworkChangedRequest) (*pb.HostNetworkChangedResponse, error) {
	log.Printf("Received HostNetworkChanged RPC")
	if s.lxd == nil {
		// LXD hasn't started yet, so nothing to do here
		log.Print("Ignoring HostNetworkChanged notification before LXD is running")
		return &pb.HostNetworkChangedResponse{}, nil
	}

	containers, err := s.lxd.GetInstances("container")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get list of containers: %v", err)
	}

	for _, container := range containers {
		// Each container's primary network interface needs to have its IPv6
		// sysctl toggled to force checking for a new address.
		c, _, err := s.lxd.GetInstanceState(container.Name)
		if err != nil {
			log.Printf("Failed to get container state for %s: %v", container.Name, err)
			continue
		}
		if c.StatusCode != api.Running {
			continue
		}

		hasPublicIPv6Address := func(state api.InstanceStateNetwork) bool {
			for _, addr := range state.Addresses {
				if addr.Family != "inet6" {
					continue
				}

				if ip := net.ParseIP(addr.Address); ip.IsGlobalUnicast() {
					return true
				}
			}

			return false
		}

		setDisableIPv6 := func(containerName, interfaceName string, disabled bool) error {
			sysctlArg := fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6", interfaceName)
			if disabled {
				sysctlArg += "=1"
			} else {
				sysctlArg += "=0"
			}

			ret, _, stderr, err := s.execProgram(container.Name, []string{"sysctl", "-w", sysctlArg})
			if err != nil {
				return fmt.Errorf("failed to run sysctl for container %s, ifname %s: %v", containerName, interfaceName, err)
			}
			if ret != 0 {
				return fmt.Errorf("failed to set sysctl for container %s, ifname %s: ret %d: %v", containerName, interfaceName, ret, stderr)
			}

			return nil
		}

		for ifname, ifstate := range c.Network {
			if !hasPublicIPv6Address(ifstate) {
				continue
			}

			// Toggle the interface's disable_ipv6 sysctl to force the interface to get a new address.
			// Don't fail on error since we want to attempt to inform every running container.
			if err := setDisableIPv6(container.Name, ifname, true); err != nil {
				log.Printf("Failed to disable IPv6 for %s:%s: %v", container.Name, ifname, err)
			}
			if err := setDisableIPv6(container.Name, ifname, false); err != nil {
				log.Printf("Failed to enable IPv6 for %s:%s: %v", container.Name, ifname, err)
			}
		}
	}

	return &pb.HostNetworkChangedResponse{}, nil
}

func (s *tremplinServer) StartLxd(ctx context.Context, in *pb.StartLxdRequest) (*pb.StartLxdResponse, error) {
	log.Print("Received StartLxd RPC")

	if s.lxd != nil {
		log.Print("LXD already running")
		return &pb.StartLxdResponse{
			Status: pb.StartLxdResponse_ALREADY_RUNNING,
		}, nil
	}

	go func() {
		if err := s.InitLxd(in.ResetLxdDb); err != nil {
			log.Printf("Starting LXD failed: %v", err)
			s.listenerClient.UpdateStartLxdStatus(context.Background(), &pb.StartLxdProgress{
				Status:        pb.StartLxdProgress_FAILED,
				FailureReason: fmt.Sprintf("Starting LXD failed: %v", err),
			})
		} else {
			s.listenerClient.UpdateStartLxdStatus(context.Background(), &pb.StartLxdProgress{
				Status: pb.StartLxdProgress_STARTED,
			})
		}
	}()
	return &pb.StartLxdResponse{
		Status: pb.StartLxdResponse_STARTING,
	}, nil
}

func (s *tremplinServer) GetDebugInfo(ctx context.Context, in *pb.GetDebugInfoRequest) (*pb.GetDebugInfoResponse, error) {
	log.Println("Received GetDebugInfo RPC")

	if s.lxd == nil {
		return &pb.GetDebugInfoResponse{
			DebugInformation: "LXD not running",
		}, nil

	}

	reader, err := s.lxd.GetInstanceLogfile(defaultContainerName, "lxc.log")
	if err != nil {
		log.Printf("Error getting container log: %v", err)
		return &pb.GetDebugInfoResponse{
			DebugInformation: fmt.Sprintf("Error getting container log: %v", err),
		}, nil
	}
	defer reader.Close()

	bytes, err := io.ReadAll(reader)
	str := string(bytes)
	if err != nil {
		str = fmt.Sprintf("Error reading log: %v. Got:\n%s", err, str)
	}
	return &pb.GetDebugInfoResponse{
		DebugInformation: str,
	}, nil
}

func (s *tremplinServer) AttachUsbToContainer(ctx context.Context, in *pb.AttachUsbToContainerRequest) (*pb.AttachUsbToContainerResponse, error) {
	log.Printf("Received AttachUsbToContainer RPC: %s %d", in.ContainerName, in.PortNum)

	m := s.usbManager
	if m == nil || s.lxd == nil {
		return &pb.AttachUsbToContainerResponse{
			Status:        pb.AttachUsbToContainerResponse_FAILED,
			FailureReason: "USB manager not ready",
		}, nil
	}

	if in.PortNum < 1 || in.PortNum > int32(len(m.portInfo)) {
		return &pb.AttachUsbToContainerResponse{
			Status:        pb.AttachUsbToContainerResponse_FAILED,
			FailureReason: fmt.Sprintf("Invalid port number %v", in.PortNum),
		}, nil
	}

	_, _, err := s.lxd.GetInstance(in.ContainerName)
	if err != nil {
		return &pb.AttachUsbToContainerResponse{
			Status:        pb.AttachUsbToContainerResponse_FAILED,
			FailureReason: fmt.Sprintf("Failed to get container %q: %v", in.ContainerName, err),
		}, nil
	}

	m.Lock()
	defer m.Unlock()
	portInfo := m.getPortInfo(int(in.PortNum))
	if portInfo.attached {
		err = portInfo.detachAllDevices(s.lxd)
		if err != nil {
			return &pb.AttachUsbToContainerResponse{
				Status:        pb.AttachUsbToContainerResponse_FAILED,
				FailureReason: fmt.Sprintf("Failed to detach USB devices from container %q: %v", portInfo.containerName, err),
			}, nil
		}
	}

	portInfo.attached = true
	portInfo.containerName = in.ContainerName

	err = portInfo.attachAllDevices(s.lxd)
	if err != nil {
		return &pb.AttachUsbToContainerResponse{
			Status:        pb.AttachUsbToContainerResponse_FAILED,
			FailureReason: fmt.Sprintf("Failed to attach USB devices to container %q: %v", in.ContainerName, err),
		}, nil
	}

	// If a device was recently attached to the VM, tremplin may not know
	// about it yet. Such devices will be processed in handleUsbUevent.

	return &pb.AttachUsbToContainerResponse{
		Status: pb.AttachUsbToContainerResponse_OK,
	}, nil
}

func (s *tremplinServer) DetachUsbFromContainer(ctx context.Context, in *pb.DetachUsbFromContainerRequest) (*pb.DetachUsbFromContainerResponse, error) {
	log.Printf("Received DetachUsbFromContainer RPC: %d", in.PortNum)

	m := s.usbManager
	if m == nil || s.lxd == nil {
		return &pb.DetachUsbFromContainerResponse{
			Status:        pb.DetachUsbFromContainerResponse_FAILED,
			FailureReason: "USB manager not ready",
		}, nil
	}

	if in.PortNum < 1 || in.PortNum > int32(len(m.portInfo)) {
		return &pb.DetachUsbFromContainerResponse{
			Status:        pb.DetachUsbFromContainerResponse_FAILED,
			FailureReason: fmt.Sprintf("Invalid port number %v", in.PortNum),
		}, nil
	}

	m.Lock()
	defer m.Unlock()
	portInfo := m.getPortInfo(int(in.PortNum))
	if portInfo.attached {
		err := portInfo.detachAllDevices(s.lxd)
		if err != nil {
			return &pb.DetachUsbFromContainerResponse{
				Status:        pb.DetachUsbFromContainerResponse_FAILED,
				FailureReason: fmt.Sprintf("Failed to detach USB devices from container %q: %v", portInfo.containerName, err),
			}, nil
		}
	}

	portInfo.attached = false
	portInfo.containerName = ""

	return &pb.DetachUsbFromContainerResponse{
		Status: pb.DetachUsbFromContainerResponse_OK,
	}, nil
}

func (s *tremplinServer) UpdateContainerDevices(ctx context.Context, in *pb.UpdateContainerDevicesRequest) (*pb.UpdateContainerDevicesResponse, error) {
	log.Printf("Received UpdateContainerDevices RPC: %s", in.ContainerName)

	response := &pb.UpdateContainerDevicesResponse{
		Results: make(map[string]pb.UpdateContainerDevicesResponse_UpdateResult),
	}
	container, etag, err := s.lxd.GetInstance(in.ContainerName)
	if container == nil {
		response.Status = pb.UpdateContainerDevicesResponse_NO_SUCH_CONTAINER
		return response, nil
	}
	containerPut := container.Writable()

	for device, action := range in.Updates {
		result := pb.UpdateContainerDevicesResponse_NO_SUCH_VM_DEVICE
		switch device {
		case strings.ToLower(pb.VmDevice_MICROPHONE.String()):
			micFilter := func(path string) bool { return micRegex.MatchString(path) }
			var count int
			switch action {
			case pb.VmDeviceAction_ENABLE:
				count = addFilteredDevices(containerPut.Devices, micFilter)
			case pb.VmDeviceAction_DISABLE:
				count = removeFilteredDevices(containerPut.Devices, micFilter)
			}
			if count > 0 {
				// The success value can be overturned if the subsequent container
				// update fails.
				result = pb.UpdateContainerDevicesResponse_SUCCESS
			}

		case strings.ToLower(pb.VmDevice_CAMERA.String()):
			// This will change when there is camera support in the Termina VM.
			result = pb.UpdateContainerDevicesResponse_NO_SUCH_VM_DEVICE
		default:
		}
		response.Results[device] = result
	}
	op, err := s.lxd.UpdateInstance(container.Name, containerPut, etag)
	doOp := func() {
		if err != nil {
			response.Status = pb.UpdateContainerDevicesResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to update container: %v", err)
			return
		}
		if err = op.Wait(); err != nil {
			response.Status = pb.UpdateContainerDevicesResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to wait for container update: %v", err)
			return
		}
		opAPI := op.Get()
		if opAPI.StatusCode != api.Success {
			response.Status = pb.UpdateContainerDevicesResponse_FAILED
			response.FailureReason = fmt.Sprintf("failed to update container devices: %v", err)
			return
		}
	}
	doOp()
	if response.Status == pb.UpdateContainerDevicesResponse_FAILED {
		// Flip any individual success results to UPDATE_FAILED
		for k, v := range response.Results {
			if v == pb.UpdateContainerDevicesResponse_SUCCESS {
				response.Results[k] = pb.UpdateContainerDevicesResponse_UPDATE_FAILED
			}
		}
	} else {
		response.Status = pb.UpdateContainerDevicesResponse_OK
	}
	return response, err
}
