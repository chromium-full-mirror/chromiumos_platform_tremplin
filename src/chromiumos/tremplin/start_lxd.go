// Copyright 2020 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
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
	"github.com/lxc/lxd/shared/version"
	"golang.org/x/sys/unix"
	yaml "gopkg.in/yaml.v2"
)

const (
	defaultStoragePoolName        = "default"
	defaultContainerName          = "penguin"
	defaultProfileName            = "default"
	defaultNetworkName            = "lxdbr0"
	defaultListenPort             = 8890
	defaultHostPort               = "7778"
	lxdConfPath                   = "/mnt/stateful/lxd_conf" // path for holding LXD client configuration
	milestonePath                 = "/run/cros_milestone"    // path to the file containing the Chrome OS milestone
	ueventBufferSize              = 4096                     // largest allowed uevent message size
	lxdDatabasePath               = "/mnt/stateful/lxd/database"
	lxdNetworkPath                = "/mnt/stateful/lxd/networks"
	lxdBackupPath                 = ".old"
	lxdVersionLastWithOldRecovery = "4.0.8"
	sysfsDir                      = "/sys"
)

const defaultNetworkTemplate = `
config:
  ipv4.address: "%s"
  ipv4.dhcp.expiry: infinite
  ipv6.address: none
  raw.dnsmasq: |-
    resolv-file=/run/resolv.conf
    dhcp-authoritative
    no-ping
    addn-hosts=/etc/arc_host.conf
managed: true
name: lxdbr0
type: bridge
`

const defaultProfileConfig = `
config:
  boot.autostart: "false"
  boot.host_shutdown_timeout: "9"
  raw.idmap: |-
    both 1000 1000
    both 655360 655360
    both 665357 665357
    both 1001 1001
  security.nesting: "true"
devices:
  cros_containers:
    path: /opt/google/cros-containers
    source: /opt/google/cros-containers
    type: disk
  cros_milestone:
    path: /dev/.cros_milestone
    source: /run/cros_milestone
    type: disk
  eth0:
    nictype: bridged
    parent: lxdbr0
    type: nic
  external:
    path: /mnt/external
    source: /mnt/external
    type: disk
  fonts:
    path: /usr/share/fonts/chromeos
    source: /mnt/fonts
    type: disk
  fuse:
    mode: '0666'
    source: /dev/fuse
    type: unix-char
  root:
    path: /
    pool: default
    type: disk
  shared:
    path: /mnt/chromeos
    source: /mnt/shared
    type: disk
  tun:
    mode: '0666'
    source: /dev/net/tun
    type: unix-char
name: default
`

var (
	// Patterns of char devices in /dev that should be mapped into the container via the LXD device list.
	validContainerDevices = []*regexp.Regexp{
		regexp.MustCompile("^dri/.*$"),
		regexp.MustCompile("^snd/[stc].*$"), // control, seq, timer
		regexp.MustCompile("^snd/pcm.*p$"),  // playback only
		regexp.MustCompile("^kmsg$"),        // dmesg -w
		regexp.MustCompile("^kvm$"),
		regexp.MustCompile("^wl0$"),
	}
	micRegex *regexp.Regexp = regexp.MustCompile("snd/pcmC.D.c$") // capture only
)

type nameField struct {
	Name string
}

type backupYaml struct {
	Container nameField
	Volume    nameField
}

func initServer(c lxd.InstanceServer) error {
	server, etag, err := c.GetServer()
	if err != nil {
		return err
	}

	serverPut := server.Writable()
	serverPut.Config["core.shutdown_timeout"] = "0"

	if err := c.UpdateServer(serverPut, etag); err != nil {
		log.Print("failed to update server: ", err)
		return err
	}
	return nil
}

func defaultStoragePool() api.StoragePoolsPost {
	return api.StoragePoolsPost{
		Name:           defaultStoragePoolName,
		Driver:         "btrfs",
		StoragePoolPut: api.StoragePoolPut{Config: map[string]string{"source": "/mnt/stateful/lxd/storage-pools/default"}},
	}
}

func initStoragePool(c lxd.InstanceServer) error {
	if _, _, err := c.GetStoragePool(defaultStoragePoolName); err != nil {
		log.Print("failed to get storage pool: ", err)
		err = c.CreateStoragePool(defaultStoragePool())
		if err != nil {
			log.Print("failed to create storage pool: ", err)
			newer, verr := lxdNewerThan(lxdVersionLastWithOldRecovery)
			if verr != nil {
				return fmt.Errorf("unable to determine lxd version: %w", verr)
			}
			if newer {
				return recoverStoragePools(c)
			}
			return err
		}
	}
	return nil
}

func initNetwork(c lxd.InstanceServer, subnet string) error {
	var defaultNetwork api.NetworksPost
	if err := yaml.Unmarshal([]byte(fmt.Sprintf(defaultNetworkTemplate, subnet)), &defaultNetwork); err != nil {
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
		log.Printf("device %v stat failed: %v", path, err)
		return err
	}
	major := unix.Major(stat.Rdev)
	minor := unix.Minor(stat.Rdev)

	var deviceType string
	fileType := stat.Mode & unix.S_IFMT
	if fileType == unix.S_IFCHR {
		deviceType = "unix-char"
	} else if fileType == unix.S_IFBLK {
		deviceType = "unix-block"
	} else {
		return fmt.Errorf("invalid device type %v", fileType)
	}

	devices[path] = map[string]string{
		"path":  path,
		"major": fmt.Sprintf("%v", major),
		"minor": fmt.Sprintf("%v", minor),
		"mode":  "0666",
		"type":  deviceType,
	}
	return nil
}

func removeDevice(devName string, devices map[string]map[string]string) error {
	path := "/dev/" + devName
	log.Print("Removing device: ", path)
	delete(devices, path)

	return nil
}

func removeFilteredDevices(devices map[string]map[string]string, filter func(path string) bool) int {
	var count int
	for path := range devices {
		if filter(path) {
			log.Print("Removing device: ", path)
			delete(devices, path)
			count++
		}
	}
	return count
}

func validContainerDevice(path string) bool {
	for _, re := range validContainerDevices {
		if re.MatchString(path) {
			return true
		}
	}
	log.Printf("Ignoring device %v", path)
	return false
}

func addFilteredDevices(devices map[string]map[string]string, filter func(path string) bool) int {
	var count int
	filepath.Walk("/dev", func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if f.Mode()&os.ModeCharDevice != os.ModeCharDevice {
			return nil
		}
		devName := strings.TrimPrefix(path, "/dev/")
		if filter(devName) {
			log.Print("Adding device: ", path)
			addDevice(devName, devices)
			count++
		}
		return nil
	})
	return count
}

func initDevices(devices map[string]map[string]string) {
	log.Print("Scanning for initial set of devices")
	addFilteredDevices(devices, validContainerDevice)
	log.Print("Device scan complete")
}

func createUeventSocket() (int, error) {
	sock, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		log.Print("could not create uevent netlink socket: ", err)
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
		log.Print("could not bind uevent netlink socket: ", err)
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

func (s *tremplinServer) ueventListen() error {
	log.Print("Listening for device updates via uevent")

	for {
		ueventBytes := make([]byte, ueventBufferSize)
		recvLen, _, err := syscall.Recvfrom(s.ueventSocket, ueventBytes, 0)
		if err != nil {
			log.Fatal("failed to read uevent: ", err)
		}

		uevent, err := readUevent(ueventBytes[:recvLen])
		if err != nil {
			log.Print("parsing uevent failed: ", err)
			continue
		}

		subsystem, ok := uevent["SUBSYSTEM"]
		if !ok {
			continue
		}

		if subsystem == "block" || subsystem == "tty" || subsystem == "usb" {
			err = s.handleUsbUevent(uevent)
		}
		if err != nil {
			log.Print("failed to handle uevent: ", err)
			continue
		}
	}
}

func initProfile(c lxd.InstanceServer) error {
	var defaultProfile api.ProfilesPost
	// NOTE: If you change idmap then you must also update the map in
	// container_file_server.go (idToContainer and idFromContainer). Also, this
	// will trigger a remap so strongly considering migrating users to shiftfs
	// beforehand.
	if err := yaml.Unmarshal([]byte(defaultProfileConfig), &defaultProfile); err != nil {
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

func updateRequiredDevices(c lxd.InstanceServer) error {
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
		return fmt.Errorf("couldn't get container names: %v", err)
	}
	for _, name := range names {
		container, etag, err := c.GetContainer(name)
		if err != nil {
			return fmt.Errorf("couldn't get container %s: %v", name, err)
		}

		containerPut := container.Writable()
		for _, name := range []string{"container_token", "ssh_authorized_keys", "ssh_host_key"} {
			if _, exists := containerPut.Devices[name]; exists {
				containerPut.Devices[name]["required"] = "false"
			}
		}

		op, err := c.UpdateContainer(name, containerPut, etag)
		if err != nil {
			return fmt.Errorf("couldn't update container %s: %v", name, err)
		}
		if err := op.Wait(); err != nil {
			return fmt.Errorf("couldn't wait to update container %s: %v", name, err)
		}
	}

	return nil
}

func (s *tremplinServer) initialSetup(c lxd.InstanceServer) error {
	usbManager := new(containerUsbManager)
	if err := initUsb(usbManager, sysfsDir); err != nil {
		// It is fine to keep going in this case. Some configurations
		// like manatee don't have USB working but the guest VM and
		// container can boot fine without it.
		log.Printf("failed to init USB manager: %v", err)
	} else {
		s.usbManager = usbManager
	}

	// Create the milestone file to bind-mount into containers.
	// This must be done before initializing the profile as LXD now checks
	// for the existence of storage volumes when the profile is set rather
	// then when the container is started.
	milestone := s.milestone

	if err := os.WriteFile(milestonePath, []byte(strconv.Itoa(milestone)), 0644); err != nil {
		return fmt.Errorf("could not write milestone file: %v", err)
	}

	if err := initServer(c); err != nil {
		return fmt.Errorf("failed to init server: %w", err)
	}

	if err := updateRequiredDevices(c); err != nil {
		return fmt.Errorf("failed to change required devices for existing containers: %w", err)
	}

	if err := initStoragePool(c); err != nil {
		return fmt.Errorf("failed to init storage pool: %w", err)
	}

	if err := initNetwork(c, s.subnet); err != nil {
		return fmt.Errorf("failed to init network: %w", err)
	}

	if err := initProfile(c); err != nil {
		return fmt.Errorf("failed to init profile: %w", err)
	}

	// Create the lxd_conf directory for manual LXD usage.
	if err := os.MkdirAll(lxdConfPath, 0755); err != nil {
		return fmt.Errorf("failed to create lxd conf dir: %w", err)
	}

	// Set the conf dir to be owned by chronos.
	u, err := user.Lookup("chronos")
	if err != nil {
		return fmt.Errorf("failed to look up chronos: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("%q is not a valid uid: %w", u.Uid, err)
	}
	g, err := user.LookupGroup("chronos")
	if err != nil {
		return fmt.Errorf("failed to look up group: %w", err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return fmt.Errorf("%q is not a valid gid: %w", g.Gid, err)
	}
	if err := os.Chown(lxdConfPath, uid, gid); err != nil {
		return fmt.Errorf("failed to chown lxd conf: %w", err)
	}

	return nil
}

// shouldResetLxdDbBeforeLaunch performs a bunch of checks to decide if we go
// ahead with wiping and recovering the LXD database. Checks for feature flag
// being enabled, only a single penguin container, that backup.yaml has the
// right name. Also returns false on error.
func (s *tremplinServer) shouldResetLxdDbBeforeLaunch() bool {
	f, err := os.Open("/mnt/stateful/lxd/containers")
	if err != nil {
		log.Print("error opening container dir to list files: ", err)
		return false
	}
	names, err := f.Readdirnames(0)
	if err != nil {
		log.Print("error listing existing containers: ", err)
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

	data, err := os.ReadFile(fmt.Sprintf("/mnt/stateful/lxd/containers/%s/backup.yaml", defaultContainerName))
	if err != nil {
		log.Print("error reading backup.yaml, not resetting since can't import without backup.yaml: ", err)
		return false
	}

	var y backupYaml
	err = yaml.Unmarshal(data, &y)
	if err != nil {
		log.Print("error unmarshalling backup.yaml, not resetting since can't perform all safety checks: ", err)
		return false
	}

	// Check if we'd hit https://github.com/lxc/lxd/issues/8071 if we continued,
	// and if so, don't continue.
	if y.Container.Name != defaultContainerName || y.Volume.Name != defaultContainerName {
		log.Printf("Container name not the same as container or volume name in backup.yaml. "+
			"Expected %q but got %q (container) and %q (volume). Not resetting as container is unimportable.",
			defaultContainerName, y.Container.Name, y.Volume.Name)
		return false
	}

	log.Printf("Resetting enabled")
	return true
}

func clearLXDDirectory(path string) error {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		// Existing folder doesn't exist, nothing to move away.
	} else if err != nil {
		// Some other error happened, no idea if the folder
		// exists or not so fail.
		return fmt.Errorf("unable to check if path %q exists: %w", path, err)
	} else {
		// Move the folder
		// Delete any old copies. We ignore errors since either it's fine
		// e.g. backup doesn't exist, or it'll cause the rename to fail.
		os.RemoveAll(path + lxdBackupPath)
		err = os.Rename(path, path+lxdBackupPath)
		if err != nil {
			return fmt.Errorf("unable to clear path %q: %w", path, err)
		}
	}
	return nil
}

func restoreLXDDirectory(path string) error {
	err := os.RemoveAll(path)
	if os.IsNotExist(err) {
		// Folder to replace doesn't exist, nothing to do.
	} else if err != nil {
		return fmt.Errorf("unable to delete the empty directory %q: %w", path, err)
	}
	err = os.Rename(path+lxdBackupPath, path)
	if err != nil {
		return fmt.Errorf("unable to restore the old path %q: %w", path, err)
	}
	return nil
}

// InitLXD sets everything up for LXD, launches it, and performs post-launch
// config such that LXD is ready for use.
func (s *tremplinServer) InitLxd(resetDB bool) error {
	// Stop LXD if it's already running (e.g. may be left over from a previous
	// failed launch).
	if err := s.StopLxdIfRunning(); err != nil {
		return fmt.Errorf("LXD is already running, but failed to stop: %w", err)
	}

	if s.ueventSocket == -1 {
		var err error
		s.ueventSocket, err = createUeventSocket()
		if err != nil {
			return fmt.Errorf("failed to open uevent netlink connection: %w", err)
		}
	} else {
		log.Print("Found an existing uevent socket so reusing. Did a previous launch fail?")
	}
	// Let the OS close the uevent socket, we keep it around for the entirety of
	// tremplin's lifetime.

	shouldReset := resetDB || s.shouldResetLxdDbBeforeLaunch()
	if shouldReset {
		log.Print("Resetting LXD DB prior to launch")
		// Move to a backup location.
		if err := clearLXDDirectory(lxdDatabasePath); err != nil {
			return err
		}
		// Clear the networks path. This is all transient
		// information, but LXD can choke on it anyway.
		if err := clearLXDDirectory(lxdNetworkPath); err != nil {
			return err
		}
	}

	c, err := s.lxdHelper.LaunchLxd()
	if err != nil {
		// Unable to launch LXD.
		if shouldReset {
			// Restore the old database.
			if err := restoreLXDDirectory(lxdDatabasePath); err != nil {
				log.Print(err)
			}
			if err := restoreLXDDirectory(lxdNetworkPath); err != nil {
				log.Print(err)
			}
		}
		return fmt.Errorf("failed to connect to LXD daemon: %w", err)
	}
	if shouldReset {
		// shouldResetLxdDbBeforeLaunch returns false if there's no penguin
		// container, or if there are any non-penguin containers. Since we made
		// it in here we know we have exactly one container to recover called
		// penguin (default name).
		err := recoverContainer(c, defaultContainerName)
		if err != nil {
			// Our preferred approach failed, let's try loading from the old
			// database as a fallback. So stop LXD, move the database back,
			// restart
			log.Print("Unable to import container: ", err)
			log.Print("Attempting fallback method of starting LXD")
			importErr := fmt.Errorf("failed to lxd import container: %w", err)
			if err := s.StopLxdIfRunning(); err != nil {
				log.Print("failed to stop LXD: ", err)
				return importErr
			}
			if err := restoreLXDDirectory(lxdDatabasePath); err != nil {
				log.Print(err)
				return importErr
			}
			if err := restoreLXDDirectory(lxdNetworkPath); err != nil {
				log.Print(err)
				return importErr
			}
			c, err = s.lxdHelper.LaunchLxd()
			if err != nil {
				log.Print("failed to connect to LXD daemon: ", err)
				return importErr
			}
			log.Print("LXD started via fallback method")
		}
	}

	if err := s.initialSetup(c); err != nil {
		return fmt.Errorf("failed initialSetup: %w", err)
	}

	if err := s.startAuditListener(); err != nil {
		return fmt.Errorf("failed to start audit listener: %w", err)
	}

	// Listen for device updates.
	s.lxd = c
	go s.ueventListen()
	go s.subscribeToEvents()
	return nil
}

// recoverContainer recovers the named container a la `lxd import`.
func recoverContainer(c lxd.InstanceServer, name string) error {
	// LXD will refuse to import a container if all its bindmounts don't exist,
	// but we don't create the files (or know what to put in them) until later.
	// We create empty files now, and then they get filled in later.
	for _, b := range getBindMounts(name, "") {
		dir := filepath.Dir(b.source)
		err := os.MkdirAll(dir, 0644)
		if err != nil {
			return fmt.Errorf("unable to create folder %s: %w", dir, err)
		}
		f, err := os.OpenFile(b.source, os.O_RDONLY|os.O_CREATE, 0644)
		if err != nil {
			return fmt.Errorf("unable to create stub file %s: %w", b.source, err)
		}
		f.Close()
	}

	newer, err := lxdNewerThan(lxdVersionLastWithOldRecovery)
	if err != nil {
		return fmt.Errorf("unable to determine lxd version: %w", err)
	}
	if newer {
		return recoverStoragePools(c)
	}
	// Old version of lxd, exec  "lxd import --force".
	out, err := execCommand("lxd", "import", "--force", name)
	if err != nil {
		return fmt.Errorf("error importing container. Stdout/err: %s. Error: %w", out, err)
	}
	return nil
}

// lxdNewerThan returns true if the current LXD version is "higher" than
// |versionString|.
func lxdNewerThan(versionString string) (bool, error) {
	minVersion, err := version.NewDottedVersion(versionString)
	if err != nil {
		log.Print("error parsing input version string: ", err)
		return false, err
	}
	out, err := execCommand("lxd", "version")
	if err != nil {
		log.Print("error calling 'lxd version': ", err)
		return false, err
	}
	log.Print("LXD reports version ", out)
	lxdVersion, err := version.NewDottedVersion(strings.TrimSpace(out))
	if err != nil {
		log.Print("error parsing lxd version: ", err)
		return false, err
	}
	return minVersion.Compare(lxdVersion) < 0, nil
}

// recoverStoragePools sends a POST to /internal/recover/import requesting the
// defaultStoragePool.
func recoverStoragePools(c lxd.InstanceServer) error {
	const recoverEndpoint = "/internal/recover/import"
	log.Printf("Recovering containers via %v", recoverEndpoint)
	type internalRecoverPost struct {
		Pools []api.StoragePoolsPost `json:"pools" yaml:"pools"`
	}
	// We should have the default pool. Make sure we recover it.
	reqRecover := internalRecoverPost{
		Pools: []api.StoragePoolsPost{defaultStoragePool()},
	}
	if _, _, err := c.RawQuery("POST", recoverEndpoint, reqRecover, ""); err != nil {
		return fmt.Errorf("error posting to %v: %w", recoverEndpoint, err)
	}
	return nil
}
