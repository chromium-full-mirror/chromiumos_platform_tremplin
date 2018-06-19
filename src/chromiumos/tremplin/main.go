// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"

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

func initNetwork(c lxd.ContainerServer) error {
	var defaultNetwork api.NetworksPost
	if err := json.Unmarshal([]byte(`{
		"name": "lxdbr0",
		"type": "bridge",
		"managed": true,
		"config": {
			"ipv4.address": "none",
			"ipv6.address": "none"
		}
	}`), &defaultNetwork); err != nil {
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
		"config": {"boot.autostart": "false"},
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

func initialSetup(c lxd.ContainerServer) error {
	if err := initStoragePool(c); err != nil {
		return err
	}

	if err := initNetwork(c); err != nil {
		return err
	}

	return initProfile(c)
}

func main() {
	c, err := lxd.ConnectLXDUnix("", nil)
	if err != nil {
		log.Fatal("Failed to connect to LXD daemon: ", err)
	}

	if err = initialSetup(c); err != nil {
		log.Fatal("Failed to set up LXD: ", err)
	}

	if len(os.Args) < 2 {
		log.Fatal("Expected an argument with the host IPv4 address")
	}

	conn, err := grpc.Dial(fmt.Sprintf("%s:%d", os.Args[1], defaultHostPort), grpc.WithInsecure())
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

	if err := server.grpcServer.Serve(lis); err != nil {
		log.Fatal("Failed to serve gRPC: ", err)
	}
}
