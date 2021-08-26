// Copyright 2019 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	pb "chromiumos/vm_tools/tremplin_proto"
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	lxd "github.com/lxc/lxd/client"
)

const upgradeScriptPath = "/opt/google/cros-containers/bin/upgrade_container"

func (s *tremplinServer) shutDownContainer(containerName string) error {
	log.Printf("shutdown")
	err := s.stopContainer(containerName)
	if err != nil {
		return fmt.Errorf("Failed to stop container %v with error %v", containerName, err)
	}
	// Notify cicerone that container has been shutdown.
	_, err = s.listenerClient.ContainerShutdown(context.Background(), &pb.ContainerShutdownInfo{ContainerName: containerName})
	if err != nil {
		return fmt.Errorf("Could not notify ContainerShutdown of %s on host: %v", containerName, err)
	}
	return nil
}

// startUpgradeContainer launches an async container upgrade job.
func (s *tremplinServer) startUpgradeContainer(containerName string, targetVersion pb.UpgradeContainerRequest_Version) (pb.UpgradeContainerResponse_Status, string) {
	if targetVersion != pb.UpgradeContainerRequest_DEBIAN_BUSTER &&
		targetVersion != pb.UpgradeContainerRequest_DEBIAN_BULLSEYE {
		return pb.UpgradeContainerResponse_NOT_SUPPORTED, fmt.Sprintf("Upgrade to %s not supported", targetVersion)
	}

	if ok := s.upgradeStatus.StartTransaction(containerName); !ok {
		return pb.UpgradeContainerResponse_ALREADY_RUNNING, fmt.Sprintf("Upgrade already in progress on %s", containerName)
	}

	osRelease, err := getGuestOSRelease(s.lxd, containerName)
	if err != nil {
		return pb.UpgradeContainerResponse_NOT_SUPPORTED, fmt.Sprintf("Failed to read /etc/os-release: %v", err)
	}

	sourceVersionString := fmt.Sprintf("DEBIAN_%s", strings.ToUpper(osRelease.versionCodename))
	sourceVersion, ok := pb.UpgradeContainerRequest_Version_value[sourceVersionString]
	if !ok || osRelease.id != "debian" {
		return pb.UpgradeContainerResponse_NOT_SUPPORTED, fmt.Sprintf("Container has unknown version %v", osRelease)
	}
	sourceVersionEnum := pb.UpgradeContainerRequest_Version(sourceVersion)

	if sourceVersionEnum > targetVersion {
		return pb.UpgradeContainerResponse_NOT_SUPPORTED, fmt.Sprintf("Container appears to have version %q, which is more recent then target version %q", sourceVersionEnum.String(), targetVersion.String())
	}

	// Find the user's home directory so we can put the log file there.
	pw_db, err := NewPasswdDatabase(s.lxd, containerName)
	if err != nil {
		return pb.UpgradeContainerResponse_FAILED, fmt.Sprintf("Failed to parse /etc/passwd in container %q: %v", containerName, err)
	}
	var homedir string
	for _, entry := range pw_db.passwd.Entries {
		if entry.Uid == primaryUserID {
			homedir = entry.Homedir
			break
		}
	}
	if homedir == "" {
		return pb.UpgradeContainerResponse_FAILED, fmt.Sprintf("Could not find primary user in container %q", containerName)
	}

	var ioSink = &stdioSink{}
	execArgs := []string{"/bin/bash", "-c"}
	bashCmd := []string{upgradeScriptPath}
	for i := sourceVersionEnum + 1; i <= targetVersion; i++ {
		bashCmd = append(bashCmd, i.String())
	}
	bashCmd = append(bashCmd, "|", "sudo", "-u", "\\#"+strconv.Itoa(primaryUserID), "-g", "\\#"+strconv.Itoa(primaryUserID), "--", "tee", "--append", homedir+"/container-upgrade.log")
	execArgs = append(execArgs, strings.Join(bashCmd, " "))

	// If we are retrying an incomplete upgrade it might look like
	// an upgrade to the same version. In that case we still want
	// to call the upgrade script with no arguments so it can
	// finish any incomplete upgrades.

	op, err := s.execProgramAsync(containerName, execArgs, ioSink, ioSink)

	if err != nil {
		s.upgradeStatus.Remove(containerName)
		return pb.UpgradeContainerResponse_FAILED, fmt.Sprintf("Error starting upgrade: %v", err)
	}

	go s.babysitContainerUpgrade(op, ioSink, containerName)
	return pb.UpgradeContainerResponse_STARTED, ""
}

// babysitContainerUpgrade waits for a container upgrade to finish, providing status updates to the listener client all the while.
// Provides a final update upon completion (success or failure) and cleans up state once the job is done.
func (s *tremplinServer) babysitContainerUpgrade(op lxd.Operation, ioSink *stdioSink, containerName string) {
	ticker := time.NewTicker(s.upgradeClientUpdateInterval)
	done := make(chan error)
	// TODO(crbug.com/930901): Timeout.
	go func() {
		for {
			select {
			case <-ticker.C:
				progress := &pb.UpgradeContainerProgress{
					ContainerName:    containerName,
					Status:           pb.UpgradeContainerProgress_IN_PROGRESS,
					ProgressMessages: strings.Split(ioSink.ReadString(), "\n"),
				}
				s.listenerClient.UpgradeContainerStatus(context.Background(), progress)
			case err := <-done:
				ticker.Stop()
				s.upgradeStatus.Remove(containerName)
				ret := op.Get().Metadata["return"].(float64)
				if err != nil || ret != 0 {
					progress := &pb.UpgradeContainerProgress{
						ContainerName:    containerName,
						Status:           pb.UpgradeContainerProgress_FAILED,
						ProgressMessages: strings.Split(ioSink.ReadString(), "\n"),
						FailureReason:    fmt.Sprintf("Upgrade ended with return code %v, lxd exec error %v", ret, err),
					}
					s.listenerClient.UpgradeContainerStatus(context.Background(), progress)
				} else {
					err = s.shutDownContainer(containerName)
					if err != nil {
						progress := &pb.UpgradeContainerProgress{
							ContainerName:    containerName,
							Status:           pb.UpgradeContainerProgress_FAILED,
							ProgressMessages: strings.Split(ioSink.ReadString(), "\n"),
							FailureReason:    fmt.Sprintf("Failed to shut down container with error %v", err),
						}
						s.listenerClient.UpgradeContainerStatus(context.Background(), progress)
						return
					}

					progress := &pb.UpgradeContainerProgress{
						ContainerName:    containerName,
						Status:           pb.UpgradeContainerProgress_SUCCEEDED,
						ProgressMessages: strings.Split(ioSink.ReadString(), "\n"),
					}
					s.listenerClient.UpgradeContainerStatus(context.Background(), progress)
				}
				return
			}
		}
	}()
	err := op.Wait()
	done <- err
}

// cancelUpgradeContainer cancels an in-progress container upgrade job.
func (s *tremplinServer) cancelUpgradeContainer(containerName string) (pb.CancelUpgradeContainerResponse_Status, string) {
	if !s.upgradeStatus.SetStatus(containerName, PendingCancel) {
		return pb.CancelUpgradeContainerResponse_NOT_RUNNING, fmt.Sprintf("Upgrade not in progress for %s", containerName)
	} else {
		// LXD doesn't support aborting an in-progress exec but that's all right, kill -9 will abort whatever we want.
		// babysitContainerUpgrade then detects that the job is no longer running and cleans up + informs the listener.
		execArgs := []string{"pkill", "-9", "-f", upgradeScriptPath}
		ret, stdout, stderr, err := s.execProgram(containerName, execArgs)
		if ret != 0 || err != nil {
			return pb.CancelUpgradeContainerResponse_FAILED, fmt.Sprintf("Error cancelling upgrade: return code: %d, error: %v, stdout: %s, stderr: %s", ret, err, stdout, stderr)
		}
		return pb.CancelUpgradeContainerResponse_CANCELLED, ""
	}
}
