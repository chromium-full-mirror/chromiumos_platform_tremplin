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
		return fmt.Errorf("Failed to change required devices for existing containers: %v", err)
	}

	if err := initStoragePool(c); err != nil {
		return err
	}

	if err := initNetwork(c, s.subnet); err != nil {
		return err
	}

	if err := initProfile(c); err != nil {
		return err
	}

	// Create the lxd_conf directory for manual LXD usage.
	if err := os.MkdirAll(lxdConfPath, 0755); err != nil {
		return err
	}

	// Set the conf dir to be owned by chronos.
	u, err := user.Lookup("chronos")
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("%q is not a valid uid: %v", u.Uid, err)
	}
	g, err := user.LookupGroup("chronos")
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("%q is not a valid gid: %v", g.Gid, err)
	}
	if err := os.Chown(lxdConfPath, uid, gid); err != nil {
		return err
	}

	return nil
}

func (s *tremplinServer) InitLxd() error {
	ueventSocket, err := createUeventSocket()
	if err != nil {
		log.Fatal("Failed to open uevent netlink connection: ", err)
	}
	// Let the OS close the uevent socket, we keep it around for the entirety of
	// tremplin's lifetime.

	c, err := s.lxdHelper.LaunchLxd()
	if err != nil {
		log.Fatal("Failed to connect to LXD daemon: ", err)
	}
	s.lxd = c

	if err := s.initialSetup(); err != nil {
		log.Fatalf("Failed initialSetup: %v", err)
		return err
	}

	if err := startAuditListener(s); err != nil {
		log.Fatalf("Failed to start audit listener: %v", err)
		return err
	}

	// Listen for device updates.
	go ueventListen(s.lxd, ueventSocket)
	return nil
}
