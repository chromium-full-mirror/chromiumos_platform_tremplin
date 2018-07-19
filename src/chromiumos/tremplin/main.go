// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/syslog"
	"net"
	"os"
	"os/user"
	"strconv"

	pb "chromiumos/vm_tools/tremplin_proto"
	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/shared/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

const (
	defaultStoragePoolName = "default"
	defaultProfileName     = "default"
	defaultNetworkName     = "lxdbr0"
	defaultListenPort      = 8890
	defaultHostPort        = 7778
	lxdConfPath            = "/mnt/stateful/lxd_conf" // path for holding LXD client configuration
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
			"ipv6.address": "none"
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
			"host-ip": {
				"source": "/run/host_ip",
				"path":   "/dev/.host_ip",
				"type":   "disk"
			},
			"sshd_config": {
				"source": "/usr/share/container_sshd_config",
				"path":   "/dev/.ssh/sshd_config",
				"type":   "disk"
			},
			"wl0": {
				"source": "/dev/wl0",
				"mode":   "0666",
				"type":   "unix-char"
			}
		}
	}`), &defaultProfile); err != nil {
		return err
	}

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

func initialSetup(c lxd.ContainerServer, subnet string) error {
	if err := initStoragePool(c); err != nil {
		return err
	}

	if err := initNetwork(c, subnet); err != nil {
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
	return os.Chown(lxdConfPath, uid, gid)
}

func main() {
	if logger, err := syslog.New(syslog.LOG_INFO, "tremplin"); err == nil {
		log.SetOutput(logger)
	}

	hostAddr := flag.String("host_addr", "", "host IP address")
	lxdSubnet := flag.String("lxd_subnet", "", "subnet for LXD in CIDR notation")
	flag.Parse()

	if len(*hostAddr) == 0 || len(*lxdSubnet) == 0 {
		log.Fatal("Both host_addr and lxd_subnet must be specified")
	}

	c, err := lxd.ConnectLXDUnix("", nil)
	if err != nil {
		log.Fatal("Failed to connect to LXD daemon: ", err)
	}

	if err = initialSetup(c, *lxdSubnet); err != nil {
		log.Fatal("Failed to set up LXD: ", err)
	}

	conn, err := grpc.Dial(fmt.Sprintf("%s:%d", *hostAddr, defaultHostPort), grpc.WithInsecure())
	if err != nil {
		log.Print("Could not connect to tremplin listener: ", err)
	}
	defer conn.Close()

	server := tremplinServer{
		lxd:            c,
		grpcServer:     grpc.NewServer(),
		listenerClient: pb.NewTremplinListenerClient(conn),
	}

	pb.RegisterTremplinServer(server.grpcServer, &server)
	reflection.Register(server.grpcServer)

	_, err = server.listenerClient.TremplinReady(context.Background(), &pb.TremplinStartupInfo{})
	if err != nil {
		log.Fatal("Failed to inform host that tremplin is ready: ", err)
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", defaultListenPort))
	if err != nil {
		log.Fatal("Failed to listen: ", err)
	}

	log.Print("tremplin ready")
	if err := server.grpcServer.Serve(lis); err != nil {
		log.Fatal("Failed to serve gRPC: ", err)
	}
}
