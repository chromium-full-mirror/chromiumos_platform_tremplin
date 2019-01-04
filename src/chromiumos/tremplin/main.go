// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"log/syslog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	pb "chromiumos/vm_tools/tremplin_proto"
	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared/api"
	"github.com/mdlayher/vsock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

const (
	defaultStoragePoolName = "default"
	defaultProfileName     = "default"
	defaultNetworkName     = "lxdbr0"
	defaultListenPort      = 8890
	defaultHostPort        = "7778"
	lxdConfPath            = "/mnt/stateful/lxd_conf" // path for holding LXD client configuration
	devDriPath             = "/dev/dri"
	milestonePath          = "/run/cros_milestone" // path to the file containing the Chrome OS milestone

	rawIDMapKey = "raw.idmap"
)

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
			"ipv6.address": "none",
			"raw.dnsmasq": "resolv-file=/run/resolv.conf\ndhcp-authoritative\nno-ping"
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

func initProfile(c lxd.ContainerServer) error {
	var defaultProfile api.ProfilesPost
	if err := json.Unmarshal([]byte(`{
		"name": "default",
		"config": {
			"boot.autostart": "false",
			"boot.host_shutdown_timeout": "9",
			"raw.idmap": "both 1000 1000\nboth 655360 655360\ngid 665357 665357",
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
			"fuse": {
				"source": "/dev/fuse",
				"mode":   "0666",
				"type":   "unix-char"
			},
			"wl0": {
				"source": "/dev/wl0",
				"mode":   "0666",
				"type":   "unix-char"
			},
			"usb": {
				"type": "usb"
			}
		}
	}`), &defaultProfile); err != nil {
		return err
	}

	filepath.Walk(devDriPath, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if path != devDriPath && f.IsDir() {
			return filepath.SkipDir
		}
		if f.Mode()&os.ModeCharDevice != os.ModeCharDevice {
			return nil
		}
		defaultProfile.Devices[f.Name()] = map[string]string{
			"source": path,
			"mode":   "0666",
			"type":   "unix-char",
		}
		return nil
	})

	profile, etag, err := c.GetProfile(defaultProfileName)
	// Assume on error that the profile doesn't exist.
	if err != nil {
		return c.CreateProfile(defaultProfile)
	}

	profilePut := profile.Writable()
	// Preserve the idmap to prevent unwanted filesystem id shifts.
	// TODO(crbug.com/911372)
	oldIDMap, ok := profilePut.Config[rawIDMapKey]

	profilePut.Config = defaultProfile.Config

	if ok {
		profilePut.Config[rawIDMapKey] = oldIDMap
	}

	profilePut.Devices = defaultProfile.Devices

	return c.UpdateProfile(defaultProfileName, profilePut, etag)
}

func initialSetup(c lxd.ContainerServer, subnet string) (int, error) {
	if err := initStoragePool(c); err != nil {
		return 0, err
	}

	if err := initNetwork(c, subnet); err != nil {
		return 0, err
	}

	if err := initProfile(c); err != nil {
		return 0, err
	}

	// Create the lxd_conf directory for manual LXD usage.
	if err := os.MkdirAll(lxdConfPath, 0755); err != nil {
		return 0, err
	}

	// Set the conf dir to be owned by chronos.
	u, err := user.Lookup("chronos")
	if err != nil {
		return 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid uid: %v", u.Uid, err)
	}
	g, err := user.LookupGroup("chronos")
	if err != nil {
		return 0, err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid gid: %v", g.Gid, err)
	}
	if err := os.Chown(lxdConfPath, uid, gid); err != nil {
		return 0, err
	}

	// Create the milestone file to bind-mount into containers.
	milestone, err := getMilestone()
	if err != nil {
		return 0, fmt.Errorf("failed to determine Chrome OS milestone: %v", err)
	}

	if err := ioutil.WriteFile(milestonePath, []byte(strconv.Itoa(milestone)), 0644); err != nil {
		return 0, fmt.Errorf("could not write milestone file: %v", err)
	}
	return milestone, nil
}

// vsockHostDialer dials the vsock host. The addr is in this case is just the
// port, as the vsock cid is implied to be the host..
func vsockHostDialer(addr string, timeout time.Duration) (net.Conn, error) {
	port, err := strconv.ParseInt(addr, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("failed to convert addr to int: %q", addr)
	}
	return vsock.Dial(vsock.ContextIDHost, uint32(port))
}

func main() {
	if logger, err := syslog.New(syslog.LOG_INFO, "tremplin"); err == nil {
		log.SetOutput(logger)
	}

	lxdSubnet := flag.String("lxd_subnet", "", "subnet for LXD in CIDR notation")
	flag.Parse()

	if len(*lxdSubnet) == 0 {
		log.Fatal("lxd_subnet must be specified")
	}

	c, err := lxd.ConnectLXDUnix("", nil)
	if err != nil {
		log.Fatal("Failed to connect to LXD daemon: ", err)
	}

	milestone, err := initialSetup(c, *lxdSubnet)
	if err != nil {
		log.Fatal("Failed to set up LXD: ", err)
	}

	conn, err := grpc.Dial(defaultHostPort,
		grpc.WithDialer(vsockHostDialer),
		grpc.WithInsecure())
	if err != nil {
		log.Print("Could not connect to tremplin listener: ", err)
	}
	defer conn.Close()

	server := tremplinServer{
		lxd:            c,
		grpcServer:     grpc.NewServer(),
		listenerClient: pb.NewTremplinListenerClient(conn),
		milestone:      milestone,
	}

	pb.RegisterTremplinServer(server.grpcServer, &server)
	reflection.Register(server.grpcServer)

	lis, err := vsock.Listen(defaultListenPort)
	if err != nil {
		log.Fatal("Failed to listen: ", err)
	}

	_, err = server.listenerClient.TremplinReady(context.Background(), &pb.TremplinStartupInfo{})
	if err != nil {
		log.Fatal("Failed to inform host that tremplin is ready: ", err)
	}

	log.Print("tremplin ready")
	if err := server.grpcServer.Serve(lis); err != nil {
		log.Fatal("Failed to serve gRPC: ", err)
	}
}
