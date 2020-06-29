// Copyright 2020 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// findLxdProcesses searches for any running LXD daemons by looking at
// the executable being run, which is symlinked at /proc/PID/exe, and
// the commandline at /proc/PID/cmdline. For LXD this will be
// /usr/sbin/lxd and {"/usr/sbin/lxd", "--group", "lxd", "--syslog"}.
func findLxdProcesses() []int {
	fileinfos, _ := ioutil.ReadDir("/proc")

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
		if dest != "/usr/sbin/lxd" {
			continue
		}
		procCmdline, err := ioutil.ReadFile("/proc/" + file.Name() + "/cmdline")
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

// StopLxdIfRunning searches for any running LXD processes and
// terminates them. This is intended to clean up any LXD processes
// that might hang around if tremplin crashes and restarts. This is
// best-effort, since it's difficult to reliably find and stop
// processes with the interface we have.
func (s *tremplinServer) StopLxdIfRunning() error {
	// Bail out early in the usual case that we don't need to do anything.
	pids := findLxdProcesses()
	if len(pids) == 0 {
		return nil
	}

	// Give LXD a chance to exit cleanly, and then SIGKILL it.
	// This will leave the containers running and LXD will
	// reconnect to them when it restarts.
	for _, pid := range pids {
		proc, _ := os.FindProcess(pid)
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("Failed to request LXD shutdown: %w", err)
		}
	}
	time.Sleep(5 * time.Second)
	for _, pid := range findLxdProcesses() {
		proc, _ := os.FindProcess(pid)
		if err := proc.Kill(); err != nil {
			return fmt.Errorf("Failed to kill LXD: %w", err)
		}
	}

	return nil
}
