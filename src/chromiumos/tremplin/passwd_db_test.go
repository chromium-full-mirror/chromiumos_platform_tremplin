// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"bytes"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"testing"

	lxd "github.com/lxc/lxd/client"
)

const testPasswd string = `root:x:0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
bin:x:2:2:bin:/bin:/usr/sbin/nologin
sys:x:3:3:sys:/dev:/usr/sbin/nologin
sync:x:4:65534:sync:/bin:/bin/sync
games:x:5:60:games:/usr/games:/usr/sbin/nologin
man:x:6:12:man:/var/cache/man:/usr/sbin/nologin
lp:x:7:7:lp:/var/spool/lpd:/usr/sbin/nologin
mail:x:8:8:mail:/var/mail:/usr/sbin/nologin
news:x:9:9:news:/var/spool/news:/usr/sbin/nologin
uucp:x:10:10:uucp:/var/spool/uucp:/usr/sbin/nologin
proxy:x:13:13:proxy:/bin:/usr/sbin/nologin
www-data:x:33:33:www-data:/var/www:/usr/sbin/nologin
backup:x:34:34:backup:/var/backups:/usr/sbin/nologin
list:x:38:38:Mailing List Manager:/var/list:/usr/sbin/nologin
irc:x:39:39:ircd:/var/run/ircd:/usr/sbin/nologin
gnats:x:41:41:Gnats Bug-Reporting System (admin):/var/lib/gnats:/usr/sbin/nologin
nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin
_apt:x:100:65534::/nonexistent:/bin/false
systemd-timesync:x:101:104:systemd Time Synchronization,,,:/run/systemd:/bin/false
systemd-network:x:102:105:systemd Network Management,,,:/run/systemd/netif:/bin/false
systemd-resolve:x:103:106:systemd Resolver,,,:/run/systemd/resolve:/bin/false
systemd-bus-proxy:x:104:107:systemd Bus Proxy,,,:/run/systemd:/bin/false
messagebus:x:105:108::/var/run/dbus:/bin/false
rtkit:x:106:109:RealtimeKit,,,:/proc:/bin/false
sshd:x:107:65534::/run/sshd:/usr/sbin/nologin
pulse:x:108:110:PulseAudio daemon,,,:/var/run/pulse:/bin/false
android-root:x:655360:655360::/dev/null:/bin/false
`

const testShadow string = `root:*:17756:0:99999:7:::
daemon:*:17756:0:99999:7:::
bin:*:17756:0:99999:7:::
sys:*:17756:0:99999:7:::
sync:*:17756:0:99999:7:::
games:*:17756:0:99999:7:::
man:*:17756:0:99999:7:::
lp:*:17756:0:99999:7:::
mail:*:17756:0:99999:7:::
news:*:17756:0:99999:7:::
uucp:*:17756:0:99999:7:::
proxy:*:17756:0:99999:7:::
www-data:*:17756:0:99999:7:::
backup:*:17756:0:99999:7:::
list:*:17756:0:99999:7:::
irc:*:17756:0:99999:7:::
gnats:*:17756:0:99999:7:::
nobody:*:17756:0:99999:7:::
_apt:*:17756:0:99999:7:::
systemd-timesync:*:17756:0:99999:7:::
systemd-network:*:17756:0:99999:7:::
systemd-resolve:*:17756:0:99999:7:::
systemd-bus-proxy:*:17756:0:99999:7:::
messagebus:*:17756:0:99999:7:::
rtkit:*:17757:0:99999:7:::
sshd:*:17757:0:99999:7:::
pulse:*:17757:0:99999:7:::
testuser:!:17814:0:99999:7:::
test:!:17862:0:99999:7:::
`

const testGroup string = `root:x:0:
daemon:x:1:
bin:x:2:
sys:x:3:
adm:x:4:
tty:x:5:
disk:x:6:
lp:x:7:
mail:x:8:
news:x:9:
uucp:x:10:
man:x:12:
proxy:x:13:
kmem:x:15:
dialout:x:20:
fax:x:21:
voice:x:22:
cdrom:x:24:
floppy:x:25:
tape:x:26:
sudo:x:27:
audio:x:29:pulse
dip:x:30:
www-data:x:33:
backup:x:34:
operator:x:37:
list:x:38:
irc:x:39:
src:x:40:
gnats:x:41:
shadow:x:42:
utmp:x:43:
video:x:44:
sasl:x:45:
plugdev:x:46:
staff:x:50:
games:x:60:
users:x:100:
nogroup:x:65534:
netdev:x:101:
ssh:x:102:
systemd-journal:x:103:
systemd-timesync:x:104:
systemd-network:x:105:
systemd-resolve:x:106:
systemd-bus-proxy:x:107:
messagebus:x:108:
rtkit:x:109:
pulse:x:110:
pulse-access:x:111:
testuser:x:1000:
`

const testGroupShadow string = `root:*::
daemon:*::
bin:*::
sys:*::
adm:*::
tty:*::
disk:*::
lp:*::
mail:*::
news:*::
uucp:*::
man:*::
proxy:*::
kmem:*::
dialout:*::testuser
fax:*::
voice:*::
cdrom:*::testuser
floppy:*::testuser
tape:*::
sudo:*::testuser
audio:*::pulse,testuser
dip:*::
www-data:*::
backup:*::
operator:*::
list:*::
irc:*::
src:*::
gnats:*::
shadow:*::
utmp:*::
video:*::testuser
sasl:*::
plugdev:*::testuser
staff:*::
games:*::
users:*::testuser
nogroup:*::
netdev:!::
ssh:!::
systemd-journal:!::
systemd-timesync:!::
systemd-network:!::
systemd-resolve:!::
systemd-bus-proxy:!::
messagebus:!::
rtkit:!::
pulse:!::
pulse-access:!::
rdma:!::
android-root:!::
android-everybody:!::testuser
testuser:!::
`

type fsMetadata struct {
	name string
	mode int
	uid  int64
	gid  int64
}

type fsEntry interface {
	Metadata() fsMetadata
}

type fakeDirectory struct {
	metadata fsMetadata
	entries  []fsEntry
}

func (d fakeDirectory) Metadata() fsMetadata {
	return d.metadata
}

type fakeFile struct {
	metadata fsMetadata
	content  []byte
}

func (f fakeFile) Metadata() fsMetadata {
	return f.metadata
}

type fakeSymlink struct {
	metadata fsMetadata
	path     string
}

func (s fakeSymlink) Metadata() fsMetadata {
	return s.metadata
}

type fakeInstanceFileServer struct {
	root fakeDirectory
	t    *testing.T
}

func (s *fakeInstanceFileServer) findEntry(filePath string, cwd *fakeDirectory) (fsEntry, error) {
	splitPath := strings.Split(filePath, "/")

	for i, component := range splitPath {
		if len(component) == 0 {
			continue
		}

		var targetEntry fsEntry
		for _, entry := range cwd.entries {
			if component == entry.Metadata().name {
				targetEntry = entry
				break
			}
		}
		if targetEntry == nil {
			return nil, errors.New("no such file or directory")
		}

		switch e := targetEntry.(type) {
		case *fakeFile:
			if i != len(splitPath)-1 {
				return nil, errors.New("not a directory")
			}
			return e, nil
		case *fakeDirectory:
			cwd = e
		case *fakeSymlink:
			if len(e.path) == 0 {
				return nil, errors.New("dangling symlink")
			}
			var symlinkTarget fsEntry
			symlinkRoot := cwd
			if e.path[0] == '/' {
				symlinkRoot = &s.root
			}

			symlinkTarget, err := s.findEntry(e.path, symlinkRoot)
			if err != nil {
				return nil, err
			}

			switch t := symlinkTarget.(type) {
			case *fakeFile:
				if i != len(splitPath)-1 {
					return nil, errors.New("not a directory")
				}
				return t, nil
			case *fakeDirectory:
				cwd = t
			case *fakeSymlink:
				return nil, errors.New("multiple symlinks not implemented")
			}
		}
	}

	return cwd, nil
}

func (s *fakeInstanceFileServer) GetInstanceFile(filePath string) (io.ReadCloser, *lxd.InstanceFileResponse, error) {
	entry, err := s.findEntry(filePath, &s.root)
	if err != nil {
		return nil, nil, err
	}

	resp := &lxd.InstanceFileResponse{
		UID:  entry.Metadata().uid,
		GID:  entry.Metadata().gid,
		Mode: entry.Metadata().mode,
	}

	var content io.ReadCloser
	switch e := entry.(type) {
	case *fakeFile:
		resp.Type = "file"
		content = io.NopCloser(bytes.NewReader(e.content))
	case *fakeDirectory:
		resp.Type = "directory"
		for _, dirEnt := range e.entries {
			resp.Entries = append(resp.Entries, dirEnt.Metadata().name)
		}
	case *fakeSymlink:
		resp.Type = "symlink"
		content = io.NopCloser(strings.NewReader(e.path))
	}

	return content, resp, nil
}

func (s *fakeInstanceFileServer) CreateInstanceFile(filePath string, args lxd.InstanceFileArgs) error {
	dirPath, filename := path.Split(filePath)

	entry, err := s.findEntry(dirPath, &s.root)
	if err != nil {
		return err
	}

	dir, ok := entry.(*fakeDirectory)
	if !ok {
		return errors.New("not a directory")
	}

	existingIndex := -1
	for i, dirEnt := range dir.entries {
		if dirEnt.Metadata().name == filename {
			existingIndex = i
			break
		}
	}

	var newEntry fsEntry
	metadata := fsMetadata{
		name: filename,
		mode: args.Mode,
		uid:  args.UID,
		gid:  args.GID,
	}
	switch args.Type {
	case "directory":
		newEntry = &fakeDirectory{
			metadata: metadata,
			entries:  []fsEntry{},
		}
	case "file":
		b, err := io.ReadAll(args.Content)
		if err != nil {
			return err
		}
		newEntry = &fakeFile{
			metadata: metadata,
			content:  b,
		}
	case "symlink":
		b, err := io.ReadAll(args.Content)
		if err != nil {
			return err
		}
		newEntry = &fakeSymlink{
			metadata: metadata,
			path:     string(b),
		}
	default:
		return errors.New("invalid file type provided")
	}

	if existingIndex != -1 {
		dir.entries[existingIndex] = newEntry
	} else {
		dir.entries = append(dir.entries, newEntry)
	}

	return nil
}

func (s *fakeInstanceFileServer) DeleteInstanceFile(filePath string) error {
	return errors.New("not yet implemented")
}

func NewFakeFileServer(t *testing.T) *fakeInstanceFileServer {
	return &fakeInstanceFileServer{
		t: t,
		root: fakeDirectory{
			metadata: fsMetadata{
				name: "",
				mode: 0755,
				uid:  0,
				gid:  0,
			},
			entries: []fsEntry{
				&fakeDirectory{
					metadata: fsMetadata{
						name: "etc",
						mode: 0755,
						uid:  0,
						gid:  0,
					},
					entries: []fsEntry{
						&fakeFile{
							metadata: fsMetadata{
								name: "passwd",
								mode: 0644,
								uid:  0,
								gid:  0,
							},
							content: []byte(testPasswd),
						},
						&fakeFile{
							metadata: fsMetadata{
								name: "shadow",
								mode: 0640,
								uid:  0,
								gid:  42,
							},
							content: []byte(testShadow),
						},
						&fakeFile{
							metadata: fsMetadata{
								name: "group",
								mode: 0644,
								uid:  0,
								gid:  0,
							},
							content: []byte(testGroup),
						},
						&fakeFile{
							metadata: fsMetadata{
								name: "gshadow",
								mode: 0640,
								uid:  0,
								gid:  42,
							},
							content: []byte(testGroupShadow),
						},
						&fakeDirectory{
							metadata: fsMetadata{
								name: "skel",
								mode: 0755,
								uid:  0,
								gid:  0,
							},
							entries: []fsEntry{
								&fakeFile{
									metadata: fsMetadata{
										name: ".bashrc",
										mode: 0644,
										uid:  0,
										gid:  0,
									},
									content: []byte("this is a bashrc"),
								},
							},
						},
					},
				},
				&fakeDirectory{
					metadata: fsMetadata{
						name: "home",
						mode: 0755,
						uid:  0,
						gid:  0,
					},
					entries: []fsEntry{},
				},
			},
		},
	}
}

func (s *fakeInstanceFileServer) assertContainerPath(filePath string, expect lxd.InstanceFileResponse) {
	_, resp, err := s.GetInstanceFile(filePath)
	if err != nil {
		s.t.Fatalf("Failed to get %s: %v", filePath, err)
	}
	if expect.UID != resp.UID {
		s.t.Fatalf("Failed uid check for %s: expected %d, actual %d", filePath, expect.UID, resp.UID)
	}
	if expect.GID != resp.GID {
		s.t.Fatalf("Failed gid check for %s: expected %d, actual %d", filePath, expect.GID, resp.GID)
	}
	if expect.Mode != resp.Mode {
		s.t.Fatalf("Failed mode check for %s: expected %o, actual %o", filePath, expect.Mode, resp.Mode)
	}
	if expect.Type != resp.Type {
		s.t.Fatalf("Failed type check for %s: expected %q, actual %q", filePath, expect.Type, resp.Type)
	}

	if len(expect.Entries) == 0 {
		return
	}

	if len(expect.Entries) != len(resp.Entries) {
		s.t.Fatalf("Directory entries length mismatch: expected %d, actual %d", len(expect.Entries), len(resp.Entries))
	}

	expectEntries := append([]string{}, expect.Entries...)
	sort.Strings(expectEntries)
	respEntries := append([]string{}, resp.Entries...)
	sort.Strings(respEntries)

	for i := range expect.Entries {
		if expectEntries[i] != respEntries[i] {
			s.t.Fatalf("Found unexpected directory entry: %s", respEntries[i])
		}
	}
}

func TestFakeFileServer(t *testing.T) {
	s := NewFakeFileServer(t)

	// Check an existing file.
	s.assertContainerPath("/etc/shadow", lxd.InstanceFileResponse{
		UID:  0,
		GID:  42,
		Mode: 0640,
		Type: "file",
	})

	// Create a directory and ensure it can be read back.
	testDir := "/home/foo"
	if err := s.CreateInstanceFile(testDir, lxd.InstanceFileArgs{
		Content:   bytes.NewReader([]byte("foo")),
		UID:       int64(1000),
		GID:       int64(1000),
		Mode:      0755,
		Type:      "directory",
		WriteMode: "overwrite",
	}); err != nil {
		t.Fatalf("failed to write directory to container: %v", err)
	}

	s.assertContainerPath(testDir, lxd.InstanceFileResponse{
		UID:     1000,
		GID:     1000,
		Mode:    0755,
		Type:    "directory",
		Entries: []string{},
	})
}

func TestUserOverwrite(t *testing.T) {
	s := NewFakeFileServer(t)

	pd, err := NewPasswdDatabase(s)
	if err != nil {
		t.Fatalf("Failed to load passwd db: %v", err)
	}

	// Create a user for alice and bob and ensure it exists.
	pd.EnsureUserExists("alice", 1234, false)
	pd.EnsureUserExists("bob", 4567, false)
	if index := pd.findPasswd("alice"); index < 0 {
		t.Fatal("User alice doesn't exist")
	}
	if index := pd.findPasswd("bob"); index < 0 {
		t.Fatal("User bob doesn't exist")
	}

	// We expect this to overwrite the user alice and the bob with a
	// different uid.
	pd.EnsureUserExists("bob", 1234, false)

	// The bob user should exist, and with the new uid.
	if index := pd.findPasswd("bob"); index < 0 {
		t.Fatal("User bob doesn't exist")
	} else if pd.passwd.Entries[index].Uid != 1234 {
		t.Fatal("User bob's uid has not been overwritten")
	}

	// The alice user should have been overwritten.
	if index := pd.findPasswd("alice"); index >= 0 {
		t.Fatal("User alice exists, but should not exist")
	}
	if index := pd.findShadow("alice"); index >= 0 {
		t.Fatal("User alice exists in user shadow, but should not exist")
	}
}

func TestHomedirCreation(t *testing.T) {
	s := NewFakeFileServer(t)

	pd, err := NewPasswdDatabase(s)
	if err != nil {
		t.Fatalf("Failed to load passwd db: %v", err)
	}

	// Test that the homedir is created for a new user.
	err = pd.EnsureUserExists("testuser", 1000, true)
	if err != nil {
		t.Fatalf("Failed creating testuser: %v", err)
	}

	s.assertContainerPath("/home/testuser", lxd.InstanceFileResponse{
		UID:  1000,
		GID:  1000,
		Mode: 0755,
		Type: "directory",
	})

	// Test that the homedir is not created if the flag is not provided.
	err = pd.EnsureUserExists("testuser2", 1001, false)
	if err != nil {
		t.Fatalf("Failed creating testuser2: %v", err)
	}

	_, _, err = s.GetInstanceFile("/home/testuser2")
	if err == nil {
		t.Fatalf("Expected no homedir for testuser2")
	}
}

func TestPersistence(t *testing.T) {
	s := NewFakeFileServer(t)

	pd, err := NewPasswdDatabase(s)
	if err != nil {
		t.Fatalf("Failed to load passwd db: %v", err)
	}

	// Create a new user and group, and add the user to the group.
	pd.EnsureGroupExists("mygroup", 1234)

	err = pd.EnsureUserExists("testuser", 1000, true)
	if err != nil {
		t.Fatalf("Failed creating testuser: %v", err)
	}

	err = pd.EnsureUserInGroup("testuser", "mygroup")
	if err != nil {
		t.Fatalf("Failed putting testuser in mygroup: %v", err)
	}

	// Save the PasswdDatabase and reload it.
	if err := pd.Save(); err != nil {
		t.Fatalf("Failed to save PasswdDatabase: %v", err)
	}
	pd, err = NewPasswdDatabase(s)
	if err != nil {
		t.Fatalf("Failed to reload passwd db: %v", err)
	}

	// Check that the user exists and the group still has the user.
	group := pd.GroupForGid(1234)
	if group == nil {
		t.Fatal("Group 1234 does not exist")
	}
	if len(group.UserList) != 1 || group.UserList[0] != "testuser" {
		t.Fatal("mygroup didn't have expected testuser")
	}

	passwd := pd.PasswdForUid(1000)
	if passwd == nil {
		t.Fatal("testuser doesn't exist")
	}
}

func TestGroupAddition(t *testing.T) {
	s := NewFakeFileServer(t)

	pd, err := NewPasswdDatabase(s)
	if err != nil {
		t.Fatalf("Failed to load passwd db: %v", err)
	}

	// Create a new user and add the user to several groups.
	err = pd.EnsureUserExists("testuser", 1000, true)
	if err != nil {
		t.Fatalf("Failed creating testuser: %v", err)
	}

	groups := []string{"audio", "cdrom", "dialout", "floppy", "plugdev", "sudo", "users", "video"}
	for _, group := range groups {
		err = pd.EnsureUserInGroup("testuser", group)
		if err != nil {
			t.Fatalf("Failed putting testuser in group %q: %v", group, err)
		}
	}

	checkUserInGroup := func(user, group string) bool {
		groupIndex := pd.findGroup(group)
		if groupIndex < 0 {
			return false
		}

		for _, candidate := range pd.group.Entries[groupIndex].UserList {
			if candidate == user {
				return true
			}
		}

		return false
	}

	for _, group := range groups {
		if !checkUserInGroup("testuser", group) {
			t.Fatalf("User testuser not in group %q", group)
		}
	}
}

func TestGroupOverwrite(t *testing.T) {
	s := NewFakeFileServer(t)

	pd, err := NewPasswdDatabase(s)
	if err != nil {
		t.Fatalf("Failed to load passwd db: %v", err)
	}

	// Create a group for alice and bob and ensure it exists.
	pd.EnsureGroupExists("alice", 1234)
	pd.EnsureGroupExists("bob", 4567)
	if groupIndex := pd.findGroup("alice"); groupIndex < 0 {
		t.Fatal("Group alice doesn't exist")
	}
	if groupIndex := pd.findGroup("bob"); groupIndex < 0 {
		t.Fatal("Group bob doesn't exist")
	}

	// We expect this to overwrite the group alice and the bob with a
	// different gid.
	pd.EnsureGroupExists("bob", 1234)

	// The bob group should exist, and with the new gid.
	if groupIndex := pd.findGroup("bob"); groupIndex < 0 {
		t.Fatal("Group bob doesn't exist")
	} else if pd.group.Entries[groupIndex].Gid != 1234 {
		t.Fatal("Group bob's Gid has not been overwritten")
	}

	// The alice group should have been overwritten.
	if groupIndex := pd.findGroup("alice"); groupIndex >= 0 {
		t.Fatal("Group alice exists, but should not exist")
	}
	if groupIndex := pd.findGroupShadow("alice"); groupIndex >= 0 {
		t.Fatal("Group alice exists in group shadow, but should not exist")
	}
}
