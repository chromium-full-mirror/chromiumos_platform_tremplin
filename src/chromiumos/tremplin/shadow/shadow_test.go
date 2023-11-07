// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package shadow

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestValidPasswd(t *testing.T) {
	passwdFile := PasswdFile{}
	err := passwdFile.UnmarshalText([]byte(`
root:x:0:0:root:/root:/bin/bash
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
testuser:x:1000:1000::/home/testuser:/bin/bash
`))

	if err != nil {
		t.Errorf("Could not parse passwd file: %v", err)
	}

	if len(passwdFile.Entries) != 29 {
		t.Errorf("Expected 29 passwd entries, got %d", len(passwdFile.Entries))
	}

	// Check that root and testuser match the expected values.
	expectedRoot := PasswdEntry{
		Name:     "root",
		Password: "x",
		Uid:      0,
		Gid:      0,
		Gecos:    "root",
		Homedir:  "/root",
		Shell:    "/bin/bash",
	}
	expectedTestUser := PasswdEntry{
		Name:     "testuser",
		Password: "x",
		Uid:      1000,
		Gid:      1000,
		Gecos:    "",
		Homedir:  "/home/testuser",
		Shell:    "/bin/bash",
	}

	assertHasUser := func(needle PasswdEntry) {
		for _, entry := range passwdFile.Entries {
			if entry.Name == needle.Name {
				if entry != needle {
					t.Errorf("found user %q, but does not match expected entry", needle.Name)
				}
				return
			}
		}
		t.Errorf("could not find user %q", needle.Name)
	}

	assertHasUser(expectedRoot)
	assertHasUser(expectedTestUser)
}

func TestValidShadow(t *testing.T) {
	shadowFile := ShadowFile{}
	err := shadowFile.UnmarshalText([]byte(`
root:*:17756:0:99999:7:::
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
`))

	if err != nil {
		t.Errorf("Could not parse shadow file: %v", err)
	}

	if len(shadowFile.Entries) != 29 {
		t.Errorf("Expected 29 shadow entries, got %d", len(shadowFile.Entries))
	}

	// Check that root and testuser match the expected values.
	expectedRoot := ShadowEntry{
		Name:       "root",
		Password:   "*",
		LastChange: NewUint64(17756),
		Min:        NewUint64(0),
		Max:        NewUint64(99999),
		Warn:       NewUint64(7),
		Inactive:   nil,
		Expire:     nil,
		Reserved:   "",
	}
	expectedTestUser := ShadowEntry{
		Name:       "testuser",
		Password:   "!",
		LastChange: NewUint64(17814),
		Min:        NewUint64(0),
		Max:        NewUint64(99999),
		Warn:       NewUint64(7),
		Inactive:   nil,
		Reserved:   "",
	}

	assertHasUser := func(needle ShadowEntry) {
		for _, entry := range shadowFile.Entries {
			if entry.Name == needle.Name {
				if !reflect.DeepEqual(entry, needle) {
					t.Errorf("found user %q, but does not match expected entry: %v", needle.Name, entry)
				}
				return
			}
		}
		t.Errorf("could not find user %q", needle.Name)
	}

	assertHasUser(expectedRoot)
	assertHasUser(expectedTestUser)
}

func TestValidGroup(t *testing.T) {
	groupFile := GroupFile{}
	err := groupFile.UnmarshalText([]byte(`
root:x:0:
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
dialout:x:20:testuser
fax:x:21:
voice:x:22:
cdrom:x:24:testuser
floppy:x:25:testuser
tape:x:26:
sudo:x:27:testuser
audio:x:29:pulse,testuser
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
video:x:44:testuser
sasl:x:45:
plugdev:x:46:testuser
staff:x:50:
games:x:60:
users:x:100:testuser
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
`))

	if err != nil {
		t.Errorf("Could not parse group file: %v", err)
	}

	if len(groupFile.Entries) != 51 {
		t.Errorf("Expected 51 shadow entries, got %d", len(groupFile.Entries))
	}

	// Check that audio and testuser groups match the expected values.
	expectedAudio := GroupEntry{
		Name:     "audio",
		Password: "x",
		Gid:      29,
		UserList: []string{"pulse", "testuser"},
	}
	expectedTestUser := GroupEntry{
		Name:     "testuser",
		Password: "x",
		Gid:      1000,
		UserList: []string{},
	}

	assertHasGroup := func(needle GroupEntry) {
		for _, entry := range groupFile.Entries {
			if entry.Name == needle.Name {
				if !reflect.DeepEqual(entry, needle) {
					t.Errorf("found group %q, but does not match expected entry: %v %v", needle.Name, entry, needle)
				}
				return
			}
		}
		t.Errorf("could not find group %q", needle.Name)
	}

	assertHasGroup(expectedAudio)
	assertHasGroup(expectedTestUser)
}

func TestValidGroupShadow(t *testing.T) {
	groupShadowFile := GroupShadowFile{}
	err := groupShadowFile.UnmarshalText([]byte(`
root:*::
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
`))

	if err != nil {
		t.Errorf("Could not parse group file: %v", err)
	}

	if len(groupShadowFile.Entries) != 54 {
		t.Errorf("Expected 54 group shadow entries, got %d", len(groupShadowFile.Entries))
	}

	// Check that audio and testuser groups match the expected values.
	expectedAudio := GroupShadowEntry{
		Name:     "audio",
		Password: "*",
		Admins:   []string{},
		Members:  []string{"pulse", "testuser"},
	}
	expectedTestUser := GroupShadowEntry{
		Name:     "testuser",
		Password: "!",
		Admins:   []string{},
		Members:  []string{},
	}

	assertHasGroup := func(needle GroupShadowEntry) {
		for _, entry := range groupShadowFile.Entries {
			if entry.Name == needle.Name {
				if !reflect.DeepEqual(entry, needle) {
					t.Errorf("found group %q, but does not match expected entry: %v %v", needle.Name, entry, needle)
				}
				return
			}
		}
		t.Errorf("could not find group %q", needle.Name)
	}

	assertHasGroup(expectedAudio)
	assertHasGroup(expectedTestUser)
}

func TestNilInterface(t *testing.T) {
	err := unmarshal(strings.NewReader(``), nil)

	if err == nil {
		t.Errorf("Expected nil to fail")
	}
}

func TestNonPointerTarget(t *testing.T) {
	err := unmarshal(strings.NewReader(``), 5)

	if err == nil {
		t.Errorf("Expected non-slice to fail")
	}
}

func TestUnmarshalNonStruct(t *testing.T) {
	err := unmarshal(strings.NewReader(`
testuser:x:-1:0:gecos:/homedir
`), &[]int{})

	if err == nil {
		t.Errorf("Expected slice of non-structs to fail")
	}
}

func TestMismatchedFieldCounts(t *testing.T) {
	err := unmarshal(strings.NewReader(`
testuser:x:-1:0:gecos:/homedir
`), &[]PasswdEntry{})

	if err == nil {
		t.Errorf("Expected mismatching field counts to fail")
	}
}

func TestMismatchedFields(t *testing.T) {
	err := unmarshal(strings.NewReader(`
testuser:x:-1:0:gecos:/homedir
`), &[]PasswdEntry{})

	if err == nil {
		t.Errorf("Expected mismatching field counts to fail")
	}
}

func TestPasswdInvalidUidGid(t *testing.T) {
	err := unmarshal(strings.NewReader(`
testuser:x:-1:0:gecos:/homedir:/bin/bash
	`), &[]PasswdEntry{})

	if err == nil {
		t.Errorf("Expected negative uid to fail parsing")
	}

	err = unmarshal(strings.NewReader(`
testuser:x:0:-1:gecos:/homedir:/bin/bash
	`), &[]PasswdEntry{})

	if err == nil {
		t.Errorf("Expected negative uid to fail parsing")
	}

}

func TestPasswdNoUsername(t *testing.T) {
	err := unmarshal(strings.NewReader(`
:x:0:0:gecos:/homedir:/bin/bash
	`), &[]PasswdEntry{})

	if err == nil {
		t.Errorf("Expected no username to fail parsing")
	}
}

func TestShadowInvalidExpire(t *testing.T) {
	err := unmarshal(strings.NewReader(`
root:*:-1:0:99999:7:::
	`), &[]ShadowEntry{})

	if err == nil {
		t.Errorf("Expected negative uid to fail parsing")
	}

	err = unmarshal(strings.NewReader(`
testuser:x:0:-1:gecos:/homedir:/bin/bash
	`), &[]PasswdEntry{})

	if err == nil {
		t.Errorf("Expected negative uid to fail parsing")
	}
}

func TestInvalidFieldKind(t *testing.T) {
	err := unmarshal(strings.NewReader(`
asdf
	`), &[]struct{ x bool }{})

	if err == nil {
		t.Errorf("Expected invalid field type to fail")
	}
}

func TestInvalidPointerFieldKind(t *testing.T) {
	err := unmarshal(strings.NewReader(`
asdf
	`), &[]struct{ x *uint32 }{})

	if err == nil {
		t.Errorf("Expected invalid pointer field type to fail")
	}
}

func TestEmptySliceEntry(t *testing.T) {
	err := unmarshal(strings.NewReader(`
asdf,
	`), &[]struct{ x []string }{})

	if err == nil {
		t.Errorf("Expected empty slice entry to fail parsing")
	}
}

type ErrorReader struct{}

func (r *ErrorReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("this is an error")
}

func TestParseIOError(t *testing.T) {
	err := unmarshal(&ErrorReader{}, &[]PasswdEntry{})

	if err == nil {
		t.Errorf("Expected parsing to fail on io.Reader error")
	}
}

func TestMarshalPasswd(t *testing.T) {
	passwdFile := PasswdFile{
		Entries: []PasswdEntry{
			{
				Name:     "root",
				Password: "x",
				Uid:      0,
				Gid:      0,
				Gecos:    "root",
				Homedir:  "/root",
				Shell:    "/bin/bash",
			},
			{
				Name:     "testuser",
				Password: "x",
				Uid:      1000,
				Gid:      1000,
				Gecos:    "",
				Homedir:  "/home/testuser",
				Shell:    "/bin/bash",
			},
		},
	}

	b, err := passwdFile.MarshalText()
	if err != nil {
		t.Errorf("Could not marshal passwd: %v", err)
	}

	if !bytes.Equal(b,
		[]byte(`root:x:0:0:root:/root:/bin/bash
testuser:x:1000:1000::/home/testuser:/bin/bash
`)) {
		t.Errorf("did not get expected marshal: %q", string(b))
	}
}

func TestMarshalShadow(t *testing.T) {
	shadowFile := ShadowFile{
		Entries: []ShadowEntry{
			{
				Name:       "root",
				Password:   "*",
				LastChange: NewUint64(17756),
				Min:        NewUint64(0),
				Max:        NewUint64(99999),
				Warn:       NewUint64(7),
				Inactive:   nil,
				Expire:     nil,
				Reserved:   "",
			},
			{
				Name:       "testuser",
				Password:   "!",
				LastChange: NewUint64(17814),
				Min:        NewUint64(0),
				Max:        NewUint64(99999),
				Warn:       NewUint64(7),
				Inactive:   nil,
				Reserved:   "",
			},
		},
	}

	b, err := shadowFile.MarshalText()
	if err != nil {
		t.Errorf("Could not marshal shadow: %v", err)
	}

	if !bytes.Equal(b,
		[]byte(`root:*:17756:0:99999:7:::
testuser:!:17814:0:99999:7:::
`)) {
		t.Errorf("did not get expected marshal: %q", string(b))
	}
}

func TestMarshalGroup(t *testing.T) {
	groupFile := GroupFile{
		Entries: []GroupEntry{
			{
				Name:     "audio",
				Password: "x",
				Gid:      29,
				UserList: []string{"pulse", "testuser"},
			},
			{
				Name:     "testuser",
				Password: "x",
				Gid:      1000,
				UserList: []string{},
			},
		},
	}

	b, err := groupFile.MarshalText()
	if err != nil {
		t.Errorf("Could not marshal group: %v", err)
	}

	if !bytes.Equal(b,
		[]byte(`audio:x:29:pulse,testuser
testuser:x:1000:
`)) {
		t.Errorf("did not get expected marshal: %q", string(b))
	}
}

func TestMarshalGroupShadow(t *testing.T) {
	groupShadowFile := GroupShadowFile{
		Entries: []GroupShadowEntry{
			{
				Name:     "root",
				Password: "!",
				Admins:   []string{},
				Members:  []string{},
			},
			{
				Name:     "audio",
				Password: "*",
				Admins:   []string{},
				Members:  []string{"pulse", "testuser"},
			},
		},
	}

	b, err := groupShadowFile.MarshalText()
	if err != nil {
		t.Errorf("Could not marshal group shadow: %v", err)
	}

	if !bytes.Equal(b,
		[]byte(`root:!::
audio:*::pulse,testuser
`)) {
		t.Errorf("did not get expected marshal: %q", string(b))
	}
}

func TestMarshalIntPtr(t *testing.T) {
	x := 5
	err := marshal(&bytes.Buffer{}, &x)

	if err == nil {
		t.Errorf("Expected marshalling from an int pointer to fail")
	}
}

func TestMarshalInt(t *testing.T) {
	err := marshal(&bytes.Buffer{}, 5)

	if err == nil {
		t.Errorf("Expected marshalling from an int to fail")
	}
}

func TestMarshalNonStruct(t *testing.T) {
	err := marshal(&bytes.Buffer{}, []int{})

	if err == nil {
		t.Errorf("Expected marshalling from slice of non-structs to fail")
	}
}
