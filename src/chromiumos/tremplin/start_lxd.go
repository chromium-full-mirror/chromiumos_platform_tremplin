// Copyright 2020 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared/api"
	"golang.org/x/sys/unix"
)

const (
	defaultStoragePoolName = "default"
	defaultContainerName   = "penguin"
	defaultProfileName     = "default"
	defaultNetworkName     = "lxdbr0"
	defaultListenPort      = 8890
	defaultHostPort        = "7778"
	lxdConfPath            = "/mnt/stateful/lxd_conf" // path for holding LXD client configuration
	milestonePath          = "/run/cros_milestone"    // path to the file containing the Chrome OS milestone
	ueventBufferSize       = 4096                     // largest allowed uevent message size
)

// Patterns of char devices in /dev that should be mapped into the container via the LXD device list.
var validContainerDevices = []*regexp.Regexp{
	regexp.MustCompile("^dri/.*$"),
	regexp.MustCompile("^snd/.*$"),
	regexp.MustCompile("^tty(ACM|USB)\\d+$"),
	regexp.MustCompile("^kvm$"),
}

func initStoragePool(c lxd.ContainerServer) error {
	if _, _, err := c.GetStoragePool(defaultStoragePoolName); err == nil {
		return nil
	}
	// Assume on error that the pool doesn't exist.
	var pool api.StoragePoolsPost
	if err := json.Unmarshal([]byte(`{
		"name": "default",
		"driver": "btrfs",
		"config": {
			"source": "/mnt/stateful/lxd/storage-pools/default"
		}
	}`), &pool); err != nil {
		return err
	}

	return c.CreateStoragePool(pool)
}

func initNetwork(c lxd.ContainerServer, subnet string) error {
	var defaultNetwork api.NetworksPost
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{
		"name": "lxdbr0",
		"type": "bridge",
		"managed": true,
		"config": {
			"ipv4.address": "%s",
			"ipv4.dhcp.expiry": "infinite",
			"ipv6.address": "none",
			"raw.dnsmasq": "resolv-file=/run/resolv.conf\ndhcp-authoritative\nno-ping\naddn-hosts=/etc/arc_host.conf"
		}
	}`, subnet)), &defaultNetwork); err != nil {
		return err
	}
	network, etag, err := c.GetNetwork(defaultNetworkName)
	// Assume on error that the network doesn't exist.
	if err != nil {
		return c.CreateNetwork(defaultNetwork)
	}

	networkPut := network.Writable()
	networkPut.Config = defaultNetwork.Config
	return c.UpdateNetwork(defaultNetworkName, networkPut, etag)
}

// Apply an update from a uevent (device addition or removal) to the LXD devices map.
func addDevice(devName string, devices map[string]map[string]string) error {
	path := "/dev/" + devName
	log.Print("Adding device: ", path)

	// Add device by major/minor number to avoid errors on removal.
	// For example, if two "remove" events occur back to back,
	// the first one will try to submit a new profile with the second
	// removed device still in the devices map, which will fail if the
	// devices are added by name on the host, since the host /dev node
	// will already be gone.
	stat := syscall.Stat_t{}
	err := syscall.Stat(path, &stat)
	if err != nil {
		log.Printf("Device %v stat failed: %v", path, err)
		return err
	}
	major := unix.Major(stat.Rdev)
	minor := unix.Minor(stat.Rdev)

	devices[path] = map[string]string{
		"path":  path,
		"major": fmt.Sprintf("%v", major),
		"minor": fmt.Sprintf("%v", minor),
		"mode":  "0666",
		"type":  "unix-char",
	}
	return nil
}

func removeDevice(devName string, devices map[string]map[string]string) error {
	path := "/dev/" + devName
	log.Print("Removing device: ", path)
	delete(devices, path)

	return nil
}

func validContainerDevice(path string) bool {
	for _, re := range validContainerDevices {
		if re.MatchString(path) {
			return true
		}
	}

	return false
}

func initDevices(devices map[string]map[string]string) {
	log.Print("Scanning for initial set of devices")

	filepath.Walk("/dev", func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if f.Mode()&os.ModeCharDevice != os.ModeCharDevice {
			return nil
		}
		devName := strings.TrimPrefix(path, "/dev/")
		if validContainerDevice(devName) {
			addDevice(devName, devices)
		}
		return nil
	})

	log.Print("Device scan complete")
}

func createUeventSocket() (int, error) {
	sock, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		log.Print("Could not create uevent netlink socket: ", err)
		return -1, err
	}

	sockaddr := syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Pid:    uint32(os.Getpid()),
		Groups: 0xFFFFFFFF,
	}

	err = syscall.Bind(sock, &sockaddr)
	if err != nil {
		syscall.Close(sock)
		log.Print("Could not bind uevent netlink socket: ", err)
		return -1, err
	}

	return sock, nil
}

func readUevent(ueventBytes []byte) (map[string]string, error) {
	bufioReader := bufio.NewReader(bytes.NewReader(ueventBytes))
	uevent := make(map[string]string)

	// Skip the header.
	_, err := bufioReader.ReadString(0)
	if err != nil {
		return nil, err
	}

	// Each message consists of KEY=VALUE records delimited by NUL.
	for {
		record, err := bufioReader.ReadString(0)
		if err != nil && err != io.EOF {
			return nil, err
		}

		if len(record) >= 1 {
			// Trim trailing NUL (ReadString includes the delimiter).
			record = record[:len(record)-1]

			keyval := strings.SplitN(record, "=", 2)
			if len(keyval) != 2 {
				continue
			}

			uevent[keyval[0]] = keyval[1]
		}

		if err == io.EOF {
			return uevent, nil
		}
	}
}

func ueventListen(c lxd.ContainerServer, ueventSocket int) error {
	log.Print("Listening for device updates via uevent")

	for {
		ueventBytes := make([]byte, ueventBufferSize)
		recvLen, _, err := syscall.Recvfrom(ueventSocket, ueventBytes, 0)
		if err != nil {
			log.Fatal("Failed to read uevent: ", err)
		}

		uevent, err := readUevent(ueventBytes[:recvLen])
		if err != nil {
			log.Print("Parsing uevent failed: ", err)
			continue
		}

		action, ok := uevent["ACTION"]
		if !ok {
			continue
		}

		devName, ok := uevent["DEVNAME"]
		if !ok {
			continue
		}

		if action != "add" && action != "remove" {
			continue
		}

		if !validContainerDevice(devName) {
			log.Print("Skipping device (not on whitelist): ", devName)
			continue
		}

		profile, etag, err := c.GetProfile(defaultProfileName)
		if err != nil {
			log.Print("GetProfile failed: ", err)
			continue
		}

		profilePut := profile.Writable()

		if action == "add" {
			addDevice(devName, profilePut.Devices)
		} else if action == "remove" {
			removeDevice(devName, profilePut.Devices)
		}

		err = c.UpdateProfile(defaultProfileName, profilePut, etag)
		if err != nil {
			log.Print("UpdateProfile failed: ", err)
		}
	}
}

func initProfile(c lxd.ContainerServer) error {
	var defaultProfile api.ProfilesPost
	if err := json.Unmarshal([]byte(`{
		"name": "default",
		"config": {
			"boot.autostart": "false",
			"boot.host_shutdown_timeout": "9",
			"raw.idmap": "both 1000 1000\nboth 655360 655360\nboth 665357 665357\nboth 1001 1001",
			"security.syscalls.blacklist": "keyctl errno 38"
		},
		"devices": {
			"root": {
				"path": "/",
				"pool": "default",
				"type": "disk"
			},
			"eth0": {
				"nictype": "bridged",
				"parent":  "lxdbr0",
				"type":    "nic"
			},
			"cros_containers": {
				"source": "/opt/google/cros-containers",
				"path":   "/opt/google/cros-containers",
				"type":   "disk"
			},
			"cros_milestone": {
				"source": "/run/cros_milestone",
				"path":   "/dev/.cros_milestone",
				"type":   "disk"
			},
			"host-ip": {
				"source": "/run/host_ip",
				"path":   "/dev/.host_ip",
				"type":   "disk"
			},
			"shared": {
				"source": "/mnt/shared",
				"path":   "/mnt/chromeos",
				"type":   "disk"
			},
			"sshd_config": {
				"source": "/usr/share/container_sshd_config",
				"path":   "/dev/.ssh/sshd_config",
				"type":   "disk"
			},
			"external": {
				"source": "/mnt/external",
				"path":   "/mnt/external",
				"type":   "disk"
			},
			"fuse": {
				"source": "/dev/fuse",
				"mode":   "0666",
				"type":   "unix-char"
			},
			"tun": {
				"source": "/dev/net/tun",
				"mode":   "0666",
				"type":   "unix-char"
			},
			"wl0": {
				"source": "/dev/wl0",
				"mode":   "0666",
				"type":   "unix-char"
			},
			"usb": {
				"type": "usb",
				"mode": "0666"
			}
		}
	}`), &defaultProfile); err != nil {
		return err
	}

	initDevices(defaultProfile.Devices)

	profile, etag, err := c.GetProfile(defaultProfileName)
	// Assume on error that the profile doesn't exist.
	if err != nil {
		return c.CreateProfile(defaultProfile)
	}

	profilePut := profile.Writable()
	profilePut.Config = defaultProfile.Config
	profilePut.Devices = defaultProfile.Devices

	return c.UpdateProfile(defaultProfileName, profilePut, etag)
}

func updateRequiredDevices(c lxd.ContainerServer) error {
	// LXD now checks by default for the existence of devices
	// attached to the container when the profile attached to that
	// container is updated. Because some of the devices are only
	// created later this will cause an error when we do
	// initProfile() if any containers already exist. We now set
	// "required=false" when setting this up, but we have to handle
	// containers that already exist and have the wrong
	// configuration.
	// TODO(sidereal) This code can be removed in M84 since all users should
	// have this update by then.
	names, err := c.GetContainerNames()
	if err != nil {
		return fmt.Errorf("Couldn't get container names: %v", err)
	}
	for _, name := range names {
		container, etag, err := c.GetContainer(name)
		if err != nil {
			return fmt.Errorf("Couldn't get container %s: %v", name, err)
		}

		containerPut := container.Writable()
		for _, name := range []string{"container_token", "ssh_authorized_keys", "ssh_host_key"} {
			if _, exists := containerPut.Devices[name]; exists {
				containerPut.Devices[name]["required"] = "false"
			}
		}

		op, err := c.UpdateContainer(name, containerPut, etag)
		if err != nil {
			return fmt.Errorf("Couldn't update container %s: %v", name, err)
		}
		if err := op.Wait(); err != nil {
			return fmt.Errorf("Couldn't wait to update container %s: %v", name, err)
		}
	}

	return nil
}

func (s *tremplinServer) initialSetup() error {
	// Create the milestone file to bind-mount into containers.
	// This must be done before initializing the profile as LXD now checks
	// for the existence of storage volumes when the profile is set rather
	// then when the container is started.
	milestone := s.milestone
	c := s.lxd

	if err := ioutil.WriteFile(milestonePath, []byte(strconv.Itoa(milestone)), 0644); err != nil {
		return fmt.Errorf("could not write milestone file: %v", err)
	}

	if err := updateRequiredDevices(c); err != nil {
		return fmt.Errorf("Failed to change required devices for existing containers: %w", err)
	}

	if err := initStoragePool(c); err != nil {
		return fmt.Errorf("Failed to init storage pool: %w", err)
	}

	if err := initNetwork(c, s.subnet); err != nil {
		return fmt.Errorf("Failed to init network: %w", err)
	}

	if err := initProfile(c); err != nil {
		return fmt.Errorf("Failed to init profile: %w", err)
	}

	// Create the lxd_conf directory for manual LXD usage.
	if err := os.MkdirAll(lxdConfPath, 0755); err != nil {
		return fmt.Errorf("Failed to create lxd conf dir: %w", err)
	}

	// Set the conf dir to be owned by chronos.
	u, err := user.Lookup("chronos")
	if err != nil {
		return fmt.Errorf("Failed to look up chronos: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("%q is not a valid uid: %w", u.Uid, err)
	}
	g, err := user.LookupGroup("chronos")
	if err != nil {
		return fmt.Errorf("Failed to look up group: %w", err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("%q is not a valid gid: %w", g.Gid, err)
	}
	if err := os.Chown(lxdConfPath, uid, gid); err != nil {
		return fmt.Errorf("Failed to chown lxd conf: %w", err)
	}

	return nil
}

// shouldResetLxdDbBeforeLaunch performs a bunch of checks to decide if we go
// ahead with wiping and recovering the LXD database. Checks for feature flag
// being enabled, only a single penguin container. Also returns false on error.
func (s *tremplinServer) shouldResetLxdDbBeforeLaunch() bool {
	if !s.features.IsResetLxdOnLaunchEnabled() {
		return false
	}
	f, err := os.Open("/mnt/stateful/lxd/containers")
	if err != nil {
		log.Print("Error opening container dir to list files: ", err)
		return false
	}
	names, err := f.Readdirnames(0)
	if err != nil {
		log.Print("Error listing existing containers: ", err)
		return false
	}
	if len(names) != 1 {
		// If multiple, at least one isn't ours so skip resetting. If 0, skip
		// because there's nothing anyway and the reimport will fail.
		log.Printf("Not resetting LXD DB, found %d containers", len(names))
		return false
	}
	if names[0] != defaultContainerName {
		log.Printf("Not resetting LXD DB, container wasn't called %s", defaultContainerName)
		return false
	}
	// TODO(davidmunro): Can we detect custom containers which are called penguin?
	log.Printf("Resetting enabled")
	return true
}

// InitLXD sets everything up for LXD, launches it, and performs post-launch
// config such that LXD is ready for use.
func (s *tremplinServer) InitLxd(resetDB bool) error {
	// Stop LXD if it's already running (e.g. may be left over from a previous
	// failed launch).
	if err := s.StopLxdIfRunning(); err != nil {
		log.Fatal("LXD is already running, but failed to stop: ", err)
	}

	ueventSocket, err := createUeventSocket()
	if err != nil {
		return fmt.Errorf("Failed to open uevent netlink connection: %w", err)
	}
	// Let the OS close the uevent socket, we keep it around for the entirety of
	// tremplin's lifetime.

	shouldReset := resetDB || s.shouldResetLxdDbBeforeLaunch()
	if shouldReset {
		log.Print("Resetting LXD DB prior to launch")
		err := os.RemoveAll("/mnt/stateful/lxd/database")
		if err != nil {
			return fmt.Errorf("Unable to clear the LXD DB: %w", err)
		}
	}

	c, err := s.lxdHelper.LaunchLxd()
	if err != nil {
		return fmt.Errorf("Failed to connect to LXD daemon: %w", err)
	}
	if shouldReset {
		// shouldResetLxdDbBeforeLaunch returns false if there's no penguin
		// container, or if there are any non-penguin containers. Since we made
		// it in here we know we have exactly one container to recover called
		// penguin (default name).
		err := recoverContainer(defaultContainerName)
		if err != nil {
			// Uh oh. We deleted their existing state (e.g. list of containers)
			// but weren't able to recover it. The good news is we won't
			// accidentally overwrite it, initStoragePool, etc will fail since
			// the storage pool, etc, already exists. The bad news is we can't
			// start their container. Fail, and we'll try again next time.
			return fmt.Errorf("Failed to lxd import container: %w", err)
		}
	}
	s.lxd = c

	if err := s.initialSetup(); err != nil {
		return fmt.Errorf("Failed initialSetup: %w", err)
	}

	if err := startAuditListener(s); err != nil {
		return fmt.Errorf("Failed to start audit listener: %w", err)
	}

	// Listen for device updates.
	go ueventListen(s.lxd, ueventSocket)
	return nil
}

// recoverContainer recovers the named container a la `lxd import`.
func recoverContainer(name string) error {
	// LXD will refuse to import a container if all its bindmounts don't exist,
	// but we don't create the files (or know what to put in them) until later.
	// We create empty files now, and then they get filled in later.
	for _, b := range getBindMounts(name, "", "", "") {
		dir := filepath.Dir(b.source)
		err := os.MkdirAll(dir, 0644)
		if err != nil {
			return fmt.Errorf("Unable to create folder %s: %w", dir, err)
		}
		f, err := os.OpenFile(b.source, os.O_RDONLY|os.O_CREATE, 0644)
		if err != nil {
			return fmt.Errorf("Unable to create stub file %s: %w", b.source, err)
		}
		f.Close()
	}
	out, err := execCommand("/usr/sbin/lxd", "import", name)
	if err != nil {
		return fmt.Errorf("Error importing container. Stdout/err: %s. Error: %w", out, err)
	}
	return nil
}
