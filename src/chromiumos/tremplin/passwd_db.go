// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"bytes"
	"encoding"
	"fmt"
	"io"
	"log"
	"path"
	"time"

	"chromiumos/tremplin/shadow"

	lxd "github.com/lxc/lxd/client"
)

const (
	groupPath       = "/etc/group"
	groupShadowPath = "/etc/gshadow"
	passwdPath      = "/etc/passwd"
	shadowPath      = "/etc/shadow"
)

type PasswdDatabase struct {
	passwd      shadow.PasswdFile
	shadow      shadow.ShadowFile
	group       shadow.GroupFile
	groupShadow shadow.GroupShadowFile

	cfs InstanceFileServer
}

func daysSinceEpoch() uint64 {
	return uint64(time.Since(time.Unix(0, 0)).Hours()) / 24
}

// NewPasswdDatabase reads/writes PasswdDatabase state in a container using the
// using the provided container file server.
func NewPasswdDatabase(cfs InstanceFileServer) (*PasswdDatabase, error) {
	pd := &PasswdDatabase{
		cfs: cfs,
	}

	loadInstanceFile := func(path string, u encoding.TextUnmarshaler) error {
		r, _, err := cfs.GetInstanceFile(path)
		if err != nil {
			return fmt.Errorf("failed to find %q: %v", path, err)
		}
		defer r.Close()

		b, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("failed to read %q: %v", path, err)
		}

		if err = u.UnmarshalText(b); err != nil {
			return fmt.Errorf("failed to unmarshal %q: %v", path, err)
		}

		return nil
	}

	paths := []struct {
		path  string
		field encoding.TextUnmarshaler
	}{
		{passwdPath, &pd.passwd},
		{shadowPath, &pd.shadow},
		{groupPath, &pd.group},
		{groupShadowPath, &pd.groupShadow},
	}

	for _, path := range paths {
		if err := loadInstanceFile(path.path, path.field); err != nil {
			return nil, err
		}
	}

	// Per crbug/1216305 we might not have successfully loaded our database,
	// even though there weren't any errors. Do a quick check to see if it's
	// obviously wrong.
	found := false
	for i := 0; i < len(pd.passwd.Entries); i++ {
		entry := pd.passwd.Entries[i]
		if entry.Name == "root" {
			found = true
			break
		}
	}
	if !found {
		// We didn't find the entries we expected, assume something went wrong.
		return nil, fmt.Errorf("loaded /etc/passwd is missing expected entries. You might be hitting crbug/1216305")
	}

	return pd, nil
}

// Save persists the passwd database back to the associated container.
func (pd *PasswdDatabase) Save() error {
	writeToContainer := func(path string, m encoding.TextMarshaler, gid int64, mode int) error {
		b, err := m.MarshalText()
		if err != nil {
			return fmt.Errorf("failed to marshal for %s: %v", path, err)
		}
		if err := pd.cfs.CreateInstanceFile(path, lxd.InstanceFileArgs{
			Content:   bytes.NewReader(b),
			UID:       0,
			GID:       gid,
			Mode:      mode,
			Type:      "file",
			WriteMode: "overwrite",
		}); err != nil {
			return fmt.Errorf("failed to write to container %s: %v", path, err)
		}

		return nil
	}

	// Default the shadow group to root, but see if a shadow group exists.
	shadowGid := int64(0)
	shadowIndex := pd.findGroup("shadow")
	if shadowIndex >= 0 {
		shadowGid = int64(pd.group.Entries[shadowIndex].Gid)
	}

	paths := []struct {
		path  string
		field encoding.TextMarshaler
		gid   int64
		mode  int
	}{
		{passwdPath, &pd.passwd, 0, 0644},
		{groupPath, &pd.group, 0, 0644},
		{shadowPath, &pd.shadow, shadowGid, 0640},
		{groupShadowPath, &pd.groupShadow, shadowGid, 0640},
	}

	for _, path := range paths {
		if err := writeToContainer(path.path, path.field, path.gid, path.mode); err != nil {
			return err
		}
	}

	return nil
}

// EnsureGroupExists ensures that a group with the given name and gid exists.
// An existing group with either the same name or gid but not both will be
// deleted.
func (pd *PasswdDatabase) EnsureGroupExists(name string, gid uint32) {
	for i := 0; i < len(pd.group.Entries); {
		entry := pd.group.Entries[i]
		if entry.Gid == gid && entry.Name == name {
			return
		}
		if entry.Gid == gid || entry.Name == name {
			// Gid or name does not match. Remove conflicting group.
			log.Printf("removing group %v(gid=%v) because it conflicts with standard group %v(gid=%v)", entry.Name, entry.Gid, name, gid)
			pd.group.Entries = append(pd.group.Entries[:i], pd.group.Entries[i+1:]...)
			if shadowIndex := pd.findGroupShadow(entry.Name); shadowIndex >= 0 {
				pd.groupShadow.Entries = append(pd.groupShadow.Entries[:shadowIndex], pd.groupShadow.Entries[shadowIndex+1:]...)
			}
			continue
		}
		i++
	}

	pd.group.Entries = append(pd.group.Entries, shadow.GroupEntry{
		Name:     name,
		Password: "x",
		Gid:      gid,
		UserList: []string{},
	})

	pd.groupShadow.Entries = append(pd.groupShadow.Entries, shadow.GroupShadowEntry{
		Name:     name,
		Password: "!",
		Admins:   []string{},
		Members:  []string{},
	})
}

// EnsureUserInGroup ensures that the given user is in the provided group name.
func (pd *PasswdDatabase) EnsureUserInGroup(user, group string) error {
	groupIndex := pd.findGroup(group)
	if groupIndex < 0 {
		return fmt.Errorf("can't add user to group: group %q does not exist", group)
	}

	for _, groupUser := range pd.group.Entries[groupIndex].UserList {
		if groupUser == user {
			return nil
		}
	}

	pd.group.Entries[groupIndex].UserList = append(pd.group.Entries[groupIndex].UserList, user)

	groupShadowIndex := pd.findGroupShadow(group)
	if groupShadowIndex < 0 {
		return fmt.Errorf("can't add user to group: gshadow %q does not exist", group)
	}

	for _, groupUser := range pd.groupShadow.Entries[groupShadowIndex].Members {
		if groupUser == user {
			return nil
		}
	}

	pd.groupShadow.Entries[groupShadowIndex].Members = append(pd.groupShadow.Entries[groupShadowIndex].Members, user)

	return nil
}

func (pd *PasswdDatabase) recursiveCopy(src, dst string, uid uint32) error {
	r, s, err := pd.cfs.GetInstanceFile(src)
	if err != nil {
		return fmt.Errorf("failed to find %q: %v", src, err)
	}

	switch s.Type {
	case "file", "symlink":
		b, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("failed to read in file %q: %v", src, err)
		}

		if err := pd.cfs.CreateInstanceFile(dst, lxd.InstanceFileArgs{
			Content:   bytes.NewReader(b),
			UID:       int64(uid),
			GID:       int64(uid),
			Mode:      s.Mode,
			Type:      s.Type,
			WriteMode: "overwrite",
		}); err != nil {
			return fmt.Errorf("failed to write %s to container %q: %v", s.Type, dst, err)
		}
	case "directory":
		if err := pd.cfs.CreateInstanceFile(dst, lxd.InstanceFileArgs{
			UID:       int64(uid),
			GID:       int64(uid),
			Mode:      s.Mode,
			Type:      "directory",
			WriteMode: "overwrite",
		}); err != nil {
			return fmt.Errorf("failed to write directory to container %q: %v", dst, err)
		}
		for _, entry := range s.Entries {
			if err := pd.recursiveCopy(path.Join(src, entry),
				path.Join(dst, entry), uid); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("got unknown file type %q", s.Type)
	}

	return nil
}

// EnsureUserExists ensures that a user with the given uid and name exists. An
// existing user with either the same name or uid but not both will be deleted.
// If loginEnabled is true, then the user will have a login shell, and the home
// directory will be created if necessary.
func (pd *PasswdDatabase) EnsureUserExists(username string, uid uint32, loginEnabled bool) error {
	for i := 0; i < len(pd.passwd.Entries); {
		entry := pd.passwd.Entries[i]
		if entry.Uid == uid && entry.Name == username {
			return nil
		}
		if entry.Uid == uid || entry.Name == username {
			// Uid or name does not match. Remove conflicting user.
			log.Printf("removing user %v(uid=%v) because it conflicts with standard user %v(uid=%v)", entry.Name, entry.Uid, username, uid)
			pd.passwd.Entries = append(pd.passwd.Entries[:i], pd.passwd.Entries[i+1:]...)
			if shadowIndex := pd.findShadow(entry.Name); shadowIndex >= 0 {
				pd.shadow.Entries = append(pd.shadow.Entries[:shadowIndex], pd.shadow.Entries[shadowIndex+1:]...)
			}
			continue
		}
		i++
	}

	homedir := "/dev/null"
	shell := "/bin/false"
	if loginEnabled {
		homedir = fmt.Sprintf("/home/%s", username)
		shell = "/bin/bash"

		_, s, err := pd.cfs.GetInstanceFile(homedir)

		// If there's a non-directory file where the home
		// directory needs to go, get rid of it. If there's
		// already a directory there though, then just leave
		// it alone because the user might want to keep it.
		removeFile := err == nil && s.Type != "directory"
		createHomeDir := !(err == nil && s.Type == "directory")

		if removeFile {
			err := pd.cfs.DeleteInstanceFile(homedir)
			if err != nil {
				return fmt.Errorf("%v type file at path %v must be removed to create home directory, but could not be: %v", s.Type, homedir, err)
			}
		}

		if createHomeDir {
			if err := pd.cfs.CreateInstanceFile(homedir, lxd.InstanceFileArgs{
				UID:  int64(uid),
				GID:  int64(uid),
				Mode: 0755,
				Type: "directory",
			}); err != nil {
				return fmt.Errorf("failed to create homedir: %v", err)
			}

			// Copy home directory skeleton from /etc/skel if it exists.
			_, s, err = pd.cfs.GetInstanceFile("/etc/skel")
			if err == nil && s.Type == "directory" {
				if err := pd.recursiveCopy("/etc/skel", homedir, uid); err != nil {
					return fmt.Errorf("failed to populate homedir: %v", err)
				}
			}
		}
	}

	pd.passwd.Entries = append(pd.passwd.Entries, shadow.PasswdEntry{
		Name:     username,
		Password: "x",
		Uid:      uid,
		Gid:      uid,
		Gecos:    username,
		Homedir:  homedir,
		Shell:    shell,
	})

	pd.shadow.Entries = append(pd.shadow.Entries, shadow.ShadowEntry{
		Name:       username,
		Password:   "!",
		LastChange: shadow.NewUint64(daysSinceEpoch()),
		Min:        shadow.NewUint64(0),
		Max:        shadow.NewUint64(99999),
		Warn:       shadow.NewUint64(7),
		Inactive:   nil,
		Expire:     nil,
		Reserved:   "",
	})

	return nil
}

// PasswdForUid returns the shadow.PasswdEntry associated with a given uid.
func (pd *PasswdDatabase) PasswdForUid(uid uint32) *shadow.PasswdEntry {
	for index, entry := range pd.passwd.Entries {
		if entry.Uid == uid {
			return &pd.passwd.Entries[index]
		}
	}

	return nil
}

// GroupForUid returns the shadow.GroupEntry associated with a given gid.
func (pd *PasswdDatabase) GroupForGid(gid uint32) *shadow.GroupEntry {
	for index, entry := range pd.group.Entries {
		if entry.Gid == gid {
			return &pd.group.Entries[index]
		}
	}

	return nil
}

func (pd *PasswdDatabase) findPasswd(name string) int {
	for index, entry := range pd.passwd.Entries {
		if entry.Name == name {
			return index
		}
	}

	return -1
}

func (pd *PasswdDatabase) findShadow(name string) int {
	for index, entry := range pd.shadow.Entries {
		if entry.Name == name {
			return index
		}
	}

	return -1
}

func (pd *PasswdDatabase) findGroup(name string) int {
	for index, entry := range pd.group.Entries {
		if entry.Name == name {
			return index
		}
	}

	return -1
}

func (pd *PasswdDatabase) findGroupShadow(name string) int {
	for index, entry := range pd.groupShadow.Entries {
		if entry.Name == name {
			return index
		}
	}

	return -1
}
