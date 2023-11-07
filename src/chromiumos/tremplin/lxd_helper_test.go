// Copyright 2020 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestRunWithRetryRunsCommand(t *testing.T) {
	ran := make(chan bool, 1)
	shutdown := make(chan os.Signal)
	cmd := func() error {
		ran <- true
		return nil
	}
	runWithRetry(cmd, shutdown, 30*time.Hour, 0)
	select {
	case <-ran:
		// Success
		break
	default:
		t.Fatal("Command not executed")
	}
}

func TestRunRetriesCorrectTimesBeforeFailing(t *testing.T) {
	ran := make(chan bool, 1)
	shutdown := make(chan os.Signal)
	attempt := 0
	cmd := func() error {
		attempt++
		if attempt == 5 {
			ran <- true
		}
		return errors.New("error")
	}
	runWithRetry(cmd, shutdown, 30*time.Hour, 5)
	select {
	case <-ran:
		// Success
		break
	default:
		t.Fatalf("Command not executed, ran %d times instead of 5", attempt)
	}
}

func TestRunStopsOnShutdown(t *testing.T) {
	restarted := make(chan bool, 1)
	shutdown := make(chan os.Signal)
	close(shutdown)
	attempt := 0
	cmd := func() error {
		attempt++
		t.Logf("Attempt %d", attempt)
		if attempt > 1 {
			restarted <- true
		}
		t.Log("Ran CMD")
		return errors.New("error")
	}
	t.Log("About to run")
	runWithRetry(cmd, shutdown, 30*time.Hour, 5)
	t.Log("Ran")
	select {
	case <-restarted:
		t.Fatalf("Command ran too many times, shouldn't have been restarted")
	default:
		// Other tests check that it ran at least once.
	}
}
