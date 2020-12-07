// Copyright 2018 The Chromium OS Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/syslog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "chromiumos/vm_tools/tremplin_proto"

	"github.com/lxc/lxd/shared/api"
	"github.com/mdlayher/vsock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// vsockHostDialer dials the vsock host. The addr is in this case is just the
// port, as the vsock cid is implied to be the host.
func vsockHostDialer(addr string, timeout time.Duration) (net.Conn, error) {
	port, err := strconv.ParseInt(addr, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("failed to convert addr to int: %q", addr)
	}
	return vsock.Dial(vsock.Host, uint32(port))
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if logger, err := syslog.New(syslog.LOG_INFO, "tremplin"); err == nil {
		log.SetOutput(logger)
	}

	lxdSubnet := flag.String("lxd_subnet", "", "subnet for LXD in CIDR notation")
	var features Features
	flag.Var(&features, "feature", "feature to enable, can specify multiple times for multiple features")
	flag.Parse()
	if len(*lxdSubnet) == 0 {
		log.Fatal("lxd_subnet must be specified")
	}

	conn, err := grpc.Dial(defaultHostPort,
		grpc.WithDialer(vsockHostDialer),
		grpc.WithInsecure())
	if err != nil {
		log.Print("Could not connect to tremplin listener: ", err)
	}
	defer conn.Close()

	milestone, err := getMilestone()
	if err != nil {
		log.Fatal("Failed to determine Chrome OS milestone: ", err)
	}
	server := tremplinServer{
		subnet:                      *lxdSubnet,
		grpcServer:                  grpc.NewServer(),
		listenerClient:              pb.NewTremplinListenerClient(conn),
		milestone:                   milestone,
		exportImportStatus:          *NewTransactionMap(),
		upgradeStatus:               *NewTransactionMap(),
		upgradeClientUpdateInterval: 5 * time.Second,
		features:                    features,
		ueventSocket:                -1,
		auditClient:                 nil,
	}

	if !features.IsStartLxdEnabled() {
		if err = server.InitLxd(false); err != nil {
			log.Fatal("Failed to set up LXD: ", err)
		}
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

	// Start gRPC server in a different goroutine.
	go func() {
		log.Print("tremplin ready")
		if err := server.grpcServer.Serve(lis); err != nil {
			// Filter error messages with the following text. When the server is stopped
			// normally, this error is always returned.
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Fatal("Failed to serve gRPC: ", err)
			}
		}
	}()

	// Shut down all containers on SIGPWR.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGPWR)
	<-sigCh
	log.Print("tremplin shutting down")
	server.grpcServer.GracefulStop()

	if server.lxd != nil {
		containers, err := server.lxd.GetContainers()
		if err != nil {
			log.Fatal("Failed to get containers list for shutdown: ", err)
		}

		wg := &sync.WaitGroup{}
		for _, container := range containers {
			if container.StatusCode != api.Running {
				continue
			}
			wg.Add(1)
			// Start all poweroffs in parallel - no reason to wait.
			go func(name string) {
				defer wg.Done()
				_, _, _, err := server.execProgram(name, []string{"poweroff"})
				if err != nil {
					log.Printf("Failed to run poweroff in container %s: %v", name, err)
				}
			}(container.Name)
		}
		// Wait for poweroff commands to be run. Note that this doesn't mean the
		// containers are fully shut down yet.
		wg.Wait()
	}

	// Now signal LXD to shut down.
	server.lxdHelper.StopLxd()
}
