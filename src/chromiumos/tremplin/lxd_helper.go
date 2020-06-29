// Copyright 2020 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	lxd "github.com/lxc/lxd/client"
)

var lxdCmd = []string{"/usr/sbin/lxd", "--group", "lxd", "--syslog"}

type lxdHelper struct {
	lxd            lxd.ContainerServer
	process        *os.Process
	mutex          sync.Mutex
	shutdownSignal chan os.Signal
}

// runWithRetry runs cmd, and whenever cmd exits runs it again unless
// shutdownSignal has been closed or the command has run too many times recently.
func runWithRetry(cmd func() error, shutdownSignal chan os.Signal, failureWindow time.Duration, numFailures int) {
	var failures []time.Time
	for {
		err := cmd()
		// If we reach here LXD has stopped running or never started.
		select {
		case <-shutdownSignal:
			log.Print("Shutting down LXD")
			// We're shutting down, nothing to do here.
			return
		default:
			// If LXD isn't running, and we're not shutting down, it's an error,
			// LXD should only terminate when we're shutting down.
			log.Print("LXD died unexpectedly with error: ", err)
			for len(failures) > 0 && failures[0].Before(time.Now().Add(-1*failureWindow)) {
				failures = failures[1:]
			}
			if len(failures) >= numFailures {
				// We can't do anything with no LXD, so give up. Keep Tremplin
				// running so we can report failures when the user tries to do
				// anything, which is the best we can offer at the moment.
				log.Print("LXD keeps dying, giving up. All future calls to Tremplin will fail until VM restart.")
				return
			}
			failures = append(failures, time.Now())
		}
	}
}

// runLxd launches LXD and blocks until LXD exits. Will set l.process.
func (l *lxdHelper) runLxd() error {
	log.Printf("Running %q", strings.Join(lxdCmd, " "))
	cmd := exec.Command(lxdCmd[0], lxdCmd[1:]...)
	err := cmd.Start()
	if err == nil {
		log.Print("LXD started")
		l.process = cmd.Process
		return cmd.Wait()
	}
	return err
}

// lxdWaitready waits for LXD to be ready to handle requests.
func (l *lxdHelper) lxdWaitready() (lxd.ContainerServer, error) {
	var lastErr error
	const lxdReadyTimeout = 2 * time.Minute
	const lxdInterval = 500 * time.Millisecond
	start := time.Now()
	attempt := 0

	for time.Now().Before(start.Add(lxdReadyTimeout)) {
		attempt++
		// Wait until there's a socket with something listening at the other end.
		c, err := lxd.ConnectLXDUnix("", nil)
		if err != nil {
			if attempt%10 == 0 {
				log.Print("ConnectLXDUnix not ready yet, retrying. Error was: ", err)
			}
			time.Sleep(lxdInterval)
			lastErr = err
			continue
		}
		// Wait until the server says it's ready.
		_, _, err = c.RawQuery("GET", "/internal/ready", nil, "")
		if err != nil {
			if attempt%10 == 0 {
				log.Print("internal/ready not ready yet, retrying. Error was: ", err)
			}
			time.Sleep(lxdInterval)
			lastErr = err
			continue
		}
		// LXD is ready.
		log.Print("LXD launched")
		return c, nil
	}
	return nil, lastErr
}

// LaunchLxd with launch LXD and return a connection to the server, or an error
// if launching failed. Will respawn LXD if it ever exits.
func (l *lxdHelper) LaunchLxd() (lxd.ContainerServer, error) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.shutdownSignal == nil {
		l.shutdownSignal = make(chan os.Signal, 1)

		// Launch LXD, we allow 10 failures per 30 seconds (copied from maitre'd)
		// before giving up.
		go runWithRetry(l.runLxd, l.shutdownSignal, 30*time.Second, 10)
	}
	if l.lxd == nil {
		// Wait for LXD to be ready.
		c, err := l.lxdWaitready()
		if err != nil {
			// Timed out
			return nil, fmt.Errorf("Timed out waiting for LXD to start. Last error was: %w", err)
		}
		log.Print("LXD running and ready")
		l.lxd = c
	}
	return l.lxd, nil
}

// StopLxd signals LXD to shut down with the provided signal and stops
// respawning it automatically. Returns as soon as the signal is sent instead of
// blocking on LXD actually shutting down.
func (l *lxdHelper) StopLxd() error {
	signal := syscall.SIGPWR
	l.mutex.Lock()
	defer l.mutex.Unlock()
	log.Print("Telling LXD to shut down")
	if l.shutdownSignal != nil {
		// This can happen if we shut down before we started LXD.
		log.Print("No channel to signal LXD on (is it running?). Nothing to do.")
		return nil
	}
	l.shutdownSignal <- signal
	return l.process.Signal(signal)
}
