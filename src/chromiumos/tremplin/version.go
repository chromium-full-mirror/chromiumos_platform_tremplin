// Copyright 2018 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	pb "go.chromium.org/chromiumos/vm_tools/tremplin_proto"

	lxd "github.com/lxc/lxd/client"
)

const (
	lsbReleasePath         = "/etc/lsb-release"
	osReleasePrimaryPath   = "/etc/os-release"
	osReleaseSecondaryPath = "/usr/lib/os-release"
	milestoneKey           = "CHROMEOS_RELEASE_CHROME_MILESTONE"
)

// dequote strips shell-style quotes from a string. It does not do any
// validation of the string, and only removes all instances of single and
// double-quote characters.
func dequote(s string) string {
	return strings.Replace(strings.Replace(s, "'", "", -1), "\"", "", -1)
}

// getMilestone returns the Chrome OS milestone of the Termina VM.
func getMilestone() (int, error) {
	f, err := os.Open(lsbReleasePath)
	if err != nil {
		return 0, err
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		s := strings.Split(scanner.Text(), "=")
		if len(s) != 2 {
			return 0, errors.New("failed to parse lsb-release entry")
		}
		if s[0] == milestoneKey {
			val, err := strconv.Atoi(dequote(s[1]))
			if err != nil {
				return 0, fmt.Errorf("%q is not a valid milestone number: %v", s[1], err)
			}
			return val, nil
		}
	}
	return 0, errors.New("no milestone key in lsb-release file")
}

// OsRelease encapsulates a subset of the os-release info as documented
// at https://www.freedesktop.org/software/systemd/man/os-release.html
type OsRelease struct {
	prettyName      string
	name            string
	version         string
	versionID       string
	versionCodename string
	id              string
}

// getFileResolvingSymlinks will get the contents of a container file while
// resolving any symlinks.
func getFileResolvingSymlinks(cfs InstanceFileServer, name, p string) (io.ReadCloser, *lxd.InstanceFileResponse, error) {
	// include/linux/namei.h defines MAXSYMLINKS
	for i := 0; i < 40; i++ {
		r, resp, err := cfs.GetInstanceFile(p)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read guest path %q: %v", p, err)
		}
		if resp.Type != "symlink" {
			return r, resp, nil
		}

		b, err := io.ReadAll(r)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read symlink %q: %v", p, err)
		}
		symlink := strings.TrimSpace(string(b))
		if path.IsAbs(symlink) {
			p = symlink
		} else {
			p = path.Join(path.Dir(p), symlink)
		}
	}
	return nil, nil, errors.New("too many symlinks")
}

// getGuestOSRelease returns the os-release information for the given container.
func getGuestOSRelease(cfs InstanceFileServer, name string) (*OsRelease, error) {
	r, _, err := getFileResolvingSymlinks(cfs, name, osReleasePrimaryPath)
	if err != nil {
		// Assume on error that the primary os-release path doesn't exist.
		r, _, err = getFileResolvingSymlinks(cfs, name, osReleaseSecondaryPath)
		if err != nil {
			return nil, fmt.Errorf("failed to find os-release file: %v", err)
		}
	}
	defer r.Close()

	osRelease := &OsRelease{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		s := strings.Split(line, "=")
		if len(s) != 2 {
			return nil, fmt.Errorf("failed to parse os-release entry %q", line)
		}
		switch s[0] {
		case "PRETTY_NAME":
			osRelease.prettyName = dequote(s[1])
		case "NAME":
			osRelease.name = dequote(s[1])
		case "VERSION":
			osRelease.version = dequote(s[1])
		case "VERSION_ID":
			osRelease.versionID = dequote(s[1])
		case "VERSION_CODENAME":
			osRelease.versionCodename = dequote(s[1])
		case "ID":
			osRelease.id = dequote(s[1])
		}
	}
	return osRelease, nil
}

func (o *OsRelease) toProto() *pb.OsRelease {
	return &pb.OsRelease{
		PrettyName: o.prettyName,
		Name:       o.name,
		Version:    o.version,
		VersionId:  o.versionID,
		Id:         o.id,
	}
}

func createAptSourceList(milestone int, osVersionId string, osVersionCodename string) string {
	var list strings.Builder
	if osVersionId == "" {
		// The VERSION_ID tag is only present on stable versions of
		// debian. For testing/unstable/experimental, bookworm is the
		// most recent set of packages we ship.
		osVersionCodename = "bookworm"
	}
	list.WriteString(fmt.Sprintf("deb https://storage.googleapis.com/cros-packages/%d %s main\n", milestone, osVersionCodename))

	// On buster and bullseye, we need the backports repo.
	if osVersionCodename == "buster" || osVersionCodename == "bullseye" {
		list.WriteString(fmt.Sprintf("deb https://deb.debian.org/debian %s-backports main\n", osVersionCodename))
	}

	return list.String()
}
