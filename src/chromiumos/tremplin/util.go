// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// execCommand executes a command and returns the result.
func execCommand(name string, args ...string) (string, error) {
	log.Printf("executing command %v with %v args: %v", name, len(args), strings.Join(args, " "))
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		log.Printf("executing command %v failed with err: %v", name, err)
	}
	return string(output), err
}

// visitFiles calls the visitor for each of the files below each root.
func visitFiles(roots []string, visitor func(string, os.FileInfo, error) error) error {
	walkFunc := filepath.WalkFunc(visitor)
	for _, root := range roots {
		if err := filepath.Walk(root, walkFunc); err != nil {
			return err
		}
	}
	return nil
}

// calculateDiskSpaceInfo returns the number of files, and the sum of their sizes, for all files below each root.
func calculateDiskSpaceInfo(roots []string) (uint32, uint64, error) {
	count := uint32(0)
	size := uint64(0)
	visitor := func(path string, fi os.FileInfo, err error) error {
		if err == nil {
			fiSize := fi.Size()
			if fiSize < 0 {
				return fmt.Errorf("file (%v) size (%v) < 0", path, fiSize)
			} else {
				count += 1
				size += uint64(fiSize)
			}
		}
		return nil
	}

	if err := visitFiles(roots, visitor); err != nil {
		return 0, 0, err
	}
	return count, size, nil
}

// ensureDirectoryExists checks if a directory exists, and if it doesn't then it makes it.
func ensureDirectoryExists(dirPath string) error {
	_, err := os.Stat(dirPath)
	if os.IsNotExist(err) {
		return os.Mkdir(dirPath, os.ModeDir)
	}
	return nil
}

// ensureDirectoryDoesntExist checks if a directory exists, and if it does then it and all of its contents are removed.
func ensureDirectoryDoesntExist(dirPath string) error {
	_, err := os.Stat(dirPath)
	if err == nil || os.IsExist(err) {
		return os.RemoveAll(dirPath)
	}
	return nil
}

// getStatefulFreeSpace returns the free space in bytes of the stateful partition as reported by statfs(2).
func getStatefulFreeSpace() (uint64, error) {
	var statfs unix.Statfs_t
	if err := unix.Statfs("/mnt/stateful", &statfs); err != nil {
		return 0, fmt.Errorf("statfs failed: %w", err)
	}

	if statfs.Bsize < 0 {
		return 0, fmt.Errorf("bsize %d is less than 0", statfs.Bsize)
	}

	return uint64(statfs.Bsize) * statfs.Bavail, nil
}
