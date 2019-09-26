// Copyright 2019 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	pb "chromiumos/vm_tools/tremplin_proto"
	"context"
	"fmt"
	lxd "github.com/lxc/lxd/client"
	"time"
)

// startUpgradeContainer launches an async container upgrade job.
func (s *tremplinServer) startUpgradeContainer(containerName string, sourceVersion, targetVersion pb.UpgradeContainerRequest_Version) (pb.UpgradeContainerResponse_Status, string) {
	if sourceVersion != pb.UpgradeContainerRequest_DEBIAN_STRETCH ||
		targetVersion != pb.UpgradeContainerRequest_DEBIAN_BUSTER {
		return pb.UpgradeContainerResponse_NOT_SUPPORTED, fmt.Sprintf("Upgrade from %s to %s not supported", sourceVersion, targetVersion)
	}

	if ok := s.upgradeStatus.StartTransaction(containerName); !ok {
		return pb.UpgradeContainerResponse_ALREADY_RUNNING, fmt.Sprintf("Upgrade already in progress on %s", containerName)
	}

	var ioSink = &stdioSink{}
	execArgs := []string{"/opt/google/cros-containers/bin/upgrade_container.sh"}
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
	// TODO(crbug.com/930901): Send messages and output in addition to status.
	// TODO(crbug.com/930901): Timeout.
	go func() {
		for {
			select {
			case <-ticker.C:
				progress := &pb.UpgradeContainerProgress{
					ContainerName: containerName,
					Status:        pb.UpgradeContainerProgress_IN_PROGRESS,
				}
				s.listenerClient.UpgradeContainerStatus(context.Background(), progress)
			case err := <-done:
				ticker.Stop()
				s.upgradeStatus.Remove(containerName)
				ret := op.Get().Metadata["return"].(float64)
				if err != nil || ret != 0 {
					// TODO(crbug.com/930901): Make sure our final message to the user includes the failure reason.
					progress := &pb.UpgradeContainerProgress{
						ContainerName: containerName,
						Status:        pb.UpgradeContainerProgress_FAILED,
					}
					s.listenerClient.UpgradeContainerStatus(context.Background(), progress)
				} else {
					progress := &pb.UpgradeContainerProgress{
						ContainerName: containerName,
						Status:        pb.UpgradeContainerProgress_SUCCEEDED,
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
