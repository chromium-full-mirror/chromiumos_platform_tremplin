// Copyright 2021 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"io"
	"log"
	"os"
	"syscall"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared"
)

type InstanceFileServer interface {
	GetInstanceFile(path string) (content io.ReadCloser, resp *lxd.InstanceFileResponse, err error)
	CreateInstanceFile(path string, args lxd.InstanceFileArgs) (err error)
	DeleteInstanceFile(path string) (err error)
}

type FsInstanceFileServer struct {
	containerName string
	needsRemap    bool
}

var cfsOverride InstanceFileServer

// OverrideInstanceFileServerForTesting overrides the container file server
// that NewInstanceFileServer will create, so you can inject a
// mock/fake/stub/etc for testing.
func OverrideInstanceFileServerForTesting(cfs InstanceFileServer) {
	cfsOverride = cfs
}

// NewInstanceFileServer creates a new FsInstanceFileServer (i.e. one backed
// by the filesystem).
func NewInstanceFileServer(containerName string) InstanceFileServer {
	if cfsOverride != nil {
		return cfsOverride
	}

	// Old containers do not use idmapped mounts, so remapping is required.
	needsRemap := false
	var s syscall.Stat_t
	if err := syscall.Stat(shared.VarPath("containers", containerName, "rootfs"), &s); err != nil {
		log.Printf("Failed to check if remap needed for container %q, assuming no: %v", containerName, err)
	} else {
		if s.Uid >= lxdSubuidStart {
			needsRemap = true
		}
	}
	return &FsInstanceFileServer{
		containerName: containerName,
		needsRemap:    needsRemap,
	}
}

// mapIdToContainerNamespace maps a uid or gid from host number to container number.
func (cfs *FsInstanceFileServer) mapIdToContainerNamespace(id int) int {
	if !cfs.needsRemap || id == PrimaryUserID || id == ChronosAccessID || id == AndroidRootID || id == AndroidEverybodyID {
		return id
	}
	return id + 1000000
}

// mapIdFromContainerNamespace maps a uid or gid from container number to host number.
func (cfs *FsInstanceFileServer) mapIdFromContainerNamespace(id int) int {
	if !cfs.needsRemap || id == PrimaryUserID || id == ChronosAccessID || id == AndroidRootID || id == AndroidEverybodyID {
		return id
	} else if id < 1000000 {
		// Before we first start the container our IDs will all be unmapped
		// (e.g. /etc/skel in the container is owned by UID 0), so if our ID is
		// below 1000000 assume it's not mapped otherwise assume it is.
		// TODO(crbug/1244460): Try getting the actual UID map from LXD and using
		// that.
		return id
	}
	return id - 1000000
}

// containerPathToHost maps a path from something relative to the container root
// to an absolute path.
func (cfs *FsInstanceFileServer) containerPathToHost(path string) string {
	return shared.VarPath("containers", cfs.containerName, "rootfs", path)
}

// GetInstanceFile reads a file or directory from the container given a path
// relative to the container root. If the file is a directory then content is
// empty and resp.entries contains a list of files, otherwise content contains
// the file contents and entries is empty. If you call this on anything other
// than a file or directory (e.g. a symlink) it probably won't do what you want.
// Note: Mode inside resp is the mode you get from the stat syscall, this is
// not a FileMode, and different from what os.Stat gives you.
// Note: GID and UID in resp are container IDs (e.g. a file owned by container
// root will have UID 0, even though on the filesystem its UID will be 1000000).
func (cfs *FsInstanceFileServer) GetInstanceFile(path string) (content io.ReadCloser, resp *lxd.InstanceFileResponse, err error) {
	p := cfs.containerPathToHost(path)
	var s syscall.Stat_t
	if err := syscall.Stat(p, &s); err != nil {
		return nil, nil, err
	}
	var t string
	var entries []string
	var reader io.ReadCloser
	// Careful! s.Mode from syscall.Stat is very similar, but not quite the
	// same, as the FileMode you get from os.Stat. So we do our own masking
	// instead of using FileMode.IsDir().
	if (s.Mode & syscall.S_IFDIR) != 0 {
		t = "directory"
		list, err := os.ReadDir(p)
		if err != nil {
			return nil, nil, err
		}
		for _, l := range list {
			entries = append(entries, l.Name())
		}
		reader = nil
	} else {
		t = "file"
		entries = make([]string, 0)
		reader, err = os.Open(p)
		if err != nil {
			return nil, nil, err
		}
	}
	resp = &lxd.InstanceFileResponse{
		UID:     int64(cfs.mapIdFromContainerNamespace(int(s.Uid))),
		GID:     int64(cfs.mapIdFromContainerNamespace(int(s.Gid))),
		Mode:    int(s.Mode),
		Type:    t,
		Entries: entries,
	}
	return reader, resp, nil
}

// CreateInstanceFile creates a file or directory at the given path relative to
// the container root. if args.Type is "directory" it will create the directory
// and any parents if needed, if args.Type is "file" then a file will be created
// with the contents of args.Content, unless args.Content is nil, in which case
// the file will be created with the specified mode if it doesn't exist, or if
// the file does already exist nothing will happen.
// The mode, UID and GID of the target will always be set to that provided, even
// if the file or directory already existed. If parent directories were created
// then the parents will have the specified mode but the owner will be the user
// tremplin is running as, any parent directories which already existed will be
// unchanged.
// Note: UID and GID are container ids (e.g. root is 0, not 1000000).
// Note: Only file and directory are supported, anything else e.g. symlinks will
// panic.
// Note: if args.Type is "file" and args.Content != nil then args.WriteMode must
// be "overwrite", anything else is unsupported. If args.Type is "directory" or
// args.Type is "file" and args.Content is nil then args.WriteMode is ignored.
func (cfs *FsInstanceFileServer) CreateInstanceFile(path string, args lxd.InstanceFileArgs) (err error) {
	p := cfs.containerPathToHost(path)
	mode := os.FileMode(args.Mode)
	if args.Type == "directory" {
		if err := os.MkdirAll(p, mode); err != nil {
			return err
		}
	} else if args.Type == "file" {
		if args.Content != nil {
			if args.WriteMode != "overwrite" {
				log.Panic("Only overwrite supported by CreateInstanceFile")
			}
			var b []byte
			b, err = io.ReadAll(args.Content)
			if err != nil {
				return err
			}
			if err := os.WriteFile(p, b, mode); err != nil {
				return err
			}
		} else {
			file, err := os.OpenFile(p, os.O_CREATE, mode)
			if err != nil {
				return err
			}
			file.Close()
		}
	} else {
		log.Panic("Only file and directory types are supported by CreateInstanceFile")
	}
	if err := os.Chown(p, cfs.mapIdToContainerNamespace(int(args.UID)), cfs.mapIdToContainerNamespace(int(args.GID))); err != nil {
		return err
	}
	log.Printf("Creating container file with metadata: %+v\n", args)
	// We set the mode above, but if the file already exists its mode won't be
	// changed so we always set the mode again here.
	return os.Chmod(p, mode)
}

// DeleteInstanceFile deletes a file or directory in the container at path
// relative to the container root. If the target is a directory will also delete
// everything within the directory,
func (cfs *FsInstanceFileServer) DeleteInstanceFile(path string) (err error) {
	return os.RemoveAll(cfs.containerPathToHost(path))
}
