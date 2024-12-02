// Copyright 2020 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// findLxdProcesses searches for any running LXD daemons by looking at
// the executable being run, which is symlinked at /proc/PID/exe, and
// the commandline at /proc/PID/cmdline. For LXD this will be
// /usr/sbin/lxd or /opt/google/lxd-next/usr/bin/lxd and
// {"lxd", "--group", "lxd", "--syslog"}.
func findLxdProcesses() []int {
	fileinfos, _ := os.ReadDir("/proc")

	// /proc/pid/cmdline contains the command line as a list of null-terminated strings.
	cmdline := strings.Join(lxdCmd, "\x00") + "\x00"

	var pids []int

	for _, file := range fileinfos {
		pid, err := strconv.Atoi(file.Name())
		if err != nil {
			// File/folder name is not an integer, so skip over this path
			continue
		}
		dest, err := os.Readlink("/proc/" + file.Name() + "/exe")
		if err != nil {
			// The /proc interface unfortunately has a
			// race condition in that a process could
			// terminate between when we saw this path
			// existed and when we try to read it. If this
			// happens then we have no idea what the
			// process was, but there's obviously no need
			// to kill it, so we just skip over
			// it. Hopefully that is the only case where
			// Readlink can return an error.
			continue
		}
		if dest != "/usr/sbin/lxd" && dest != "/opt/google/lxd-next/usr/bin/lxd" {
			continue
		}
		procCmdline, err := os.ReadFile("/proc/" + file.Name() + "/cmdline")
		if err != nil {
			// As above, we just skip over this path if there's an error
			continue
		}
		if string(procCmdline) == cmdline {
			pids = append(pids, pid)
		}
	}

	return pids
}

// waitForLxdToExit blocks until there are running existing LXD processes,
// waiting up to the specified timeout. Returns nil if LXD was observed to exit,
// or a non-nil error if LXD is still running after the timeout or another error
// occurred.
func waitForLxdToExit(timeout time.Duration) error {
	end := time.Now().Add(timeout)

	for time.Now().Before(end) {
		pids := findLxdProcesses()
		if len(pids) == 0 {
			return nil
		}
	}
	return fmt.Errorf("timed out waiting for LXD to exit")
}

// StopLxdIfRunning searches for any running LXD processes and
// terminates them. This is intended to clean up any LXD processes
// that might hang around if tremplin crashes and restarts. This is
// best-effort, since it's difficult to reliably find and stop
// processes with the interface we have.
func (s *tremplinServer) StopLxdIfRunning() error {
	s.lxdHelper.StopLxd(true)
	s.lxd = nil

	if err := waitForLxdToExit(2 * time.Second); err == nil {
		// All the LXD instances have stopped.
		return nil
	}

	// Give LXD a chance to exit cleanly, and then SIGKILL it.
	// This will leave the containers running and LXD will
	// reconnect to them when it restarts.
	pids := findLxdProcesses()
	for _, pid := range pids {
		proc, _ := os.FindProcess(pid)
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			// If the process already exited at any point between listing
			// processes and here the signal will fail with a private error
			// (stopping us from using errors.Is to check). But we try again
			// harder later so just ignore errors.
			fmt.Print("Failed to request LXD shutdown: ", err)
		}
	}
	if err := waitForLxdToExit(5 * time.Second); err == nil {
		return nil
	}
	for _, pid := range pids {
		proc, _ := os.FindProcess(pid)
		if err := proc.Kill(); err != nil {
			// If the process already exited at any point between listing
			// processes and here (including in response to the previous
			// SIGTERM) the signal will fail with a private error (stopping us
			// from using errors.Is to test for it). But in practice the ways
			// kill fails (since we're running as root) are to do with the
			// process not existing, so either way we're happy.
			fmt.Print("Failed to force LXD shutdown: ", err)
		}
	}
	s.lxd = nil
	return waitForLxdToExit(5 * time.Second)
}
