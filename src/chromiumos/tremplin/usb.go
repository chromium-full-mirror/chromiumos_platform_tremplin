// Copyright 2022 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	lxd "github.com/lxc/lxd/client"
)

const (
	pciClassXhci   = "0x0c0330\n"
	usb2RootHubPid = "0002\n"
	usb3RootHubPid = "0003\n"
)

type containerUsbManager struct {
	portInfo       []containerUsbPort
	controllerPath string
	busPortRegexp  *regexp.Regexp
	usb2BusNumber  int
	usb3BusNumber  int
	usb2NumPorts   int
	usb3NumPorts   int
	lock           sync.Mutex
}

type containerUsbPort struct {
	attached      bool
	knownDevices  map[string]bool
	containerName string
}

func (u *containerUsbPort) attachAllDevices(lxd lxd.InstanceServer) error {
	container, etag, err := lxd.GetInstance(u.containerName)
	if err != nil {
		return fmt.Errorf("failed to get container %q: %w", u.containerName, err)
	}

	instancePut := container.Writable()
	for devName := range u.knownDevices {
		err = addDevice(devName, instancePut.Devices)
		if err != nil {
			return fmt.Errorf("failed to add device: %w", err)
		}
	}

	op, err := lxd.UpdateInstance(u.containerName, instancePut, etag)
	if err != nil {
		return fmt.Errorf("UpdateInstance failed %q: %w", u.containerName, err)
	}
	if err := op.Wait(); err != nil {
		return fmt.Errorf("wait for UpdateInstance failed %q: %w", u.containerName, err)
	}

	return nil
}

func (u *containerUsbPort) detachAllDevices(lxd lxd.InstanceServer) error {
	container, etag, err := lxd.GetInstance(u.containerName)
	if err != nil {
		return fmt.Errorf("failed to get container %q: %w", u.containerName, err)
	}

	instancePut := container.Writable()
	for devName := range u.knownDevices {
		err = removeDevice(devName, instancePut.Devices)
		if err != nil {
			return fmt.Errorf("failed to remove device: %w", err)
		}
	}

	op, err := lxd.UpdateInstance(u.containerName, instancePut, etag)
	if err != nil {
		return fmt.Errorf("UpdateInstance failed %q: %w", u.containerName, err)
	}
	if err := op.Wait(); err != nil {
		return fmt.Errorf("wait for UpdateInstance failed %q: %w", u.containerName, err)
	}

	return nil
}

func initUsb(m *containerUsbManager, sysfsDir string) error {
	devs, err := filepath.Glob(sysfsDir + "/bus/pci/devices/*")
	if err != nil {
		return fmt.Errorf("failed to find PCI devices")
	}

	for _, dev := range devs {
		class, err := os.ReadFile(dev + "/class")
		if err != nil {
			return fmt.Errorf("failed to read class code of %q: %w", dev, err)
		}
		if string(class) != pciClassXhci {
			continue
		}

		target, err := os.Readlink(dev)
		if err != nil {
			return fmt.Errorf("failed to read controller path: %w", err)
		}

		// Trim leading components of link target so it is relative to sysfsDir.
		m.controllerPath, err = filepath.Rel("../../../", target)
		if err != nil {
			return fmt.Errorf("failed to read controller path: %w", err)
		}

		buses, err := filepath.Glob(dev + "/usb*")
		if err != nil {
			return fmt.Errorf("failed to find USB buses")
		}

		for _, bus := range buses {
			productId, err := os.ReadFile(bus + "/idProduct")
			if err != nil {
				return fmt.Errorf("failed to read product ID: %w", err)
			}

			numPortsRaw, err := os.ReadFile(bus + "/maxchild")
			if err != nil {
				return fmt.Errorf("failed to read maxchild: %w", err)
			}
			numPorts, err := strconv.ParseInt(strings.TrimSpace(string(numPortsRaw)), 10, 0)
			if err != nil {
				return fmt.Errorf("failed to parse maxchild: %w", err)
			}

			busString := strings.TrimPrefix(bus, dev+"/usb")
			busNumber, err := strconv.ParseInt(busString, 10, 0)
			if err != nil {
				return fmt.Errorf("failed to get USB bus number")
			}

			if string(productId) == usb2RootHubPid {
				m.usb2BusNumber = int(busNumber)
				m.usb2NumPorts = int(numPorts)
			} else if string(productId) == usb3RootHubPid {
				m.usb3BusNumber = int(busNumber)
				m.usb3NumPorts = int(numPorts)
			} else {
				return fmt.Errorf("bus%d does not look like a root hub", busNumber)
			}
		}
		break
	}

	if m.controllerPath == "" {
		return fmt.Errorf("failed to find the USB controller")
	}

	if m.usb2BusNumber == 0 || m.usb3BusNumber == 0 {
		return fmt.Errorf("failed to determine USB bus numbers")
	}

	m.portInfo = make([]containerUsbPort, m.usb2NumPorts+m.usb3NumPorts)
	for i := range m.portInfo {
		m.portInfo[i].knownDevices = make(map[string]bool)
	}

	return nil
}

func (m *containerUsbManager) Lock() {
	m.lock.Lock()
}

func (m *containerUsbManager) Unlock() {
	m.lock.Unlock()
}

// Get details of a USB port attachment. Caller must hold the lock.
func (m *containerUsbManager) getPortInfo(portNum int) *containerUsbPort {
	return &m.portInfo[portNum-1]
}

// Get the logical port number from a DEVPATH. The DEVPATH must correspond to
// the device controllerPath.
func (m *containerUsbManager) getPortNumberFromDevPath(devPath string) (int, error) {
	if m.busPortRegexp == nil {
		var err error
		m.busPortRegexp, err = regexp.Compile(fmt.Sprintf(`^/%s/usb\d/(\d)-(\d)`, m.controllerPath))
		if err != nil {
			return 0, err
		}
	}

	busPort := m.busPortRegexp.FindStringSubmatch(devPath)
	if busPort == nil {
		return 0, fmt.Errorf("invalid bus/port")
	}

	b, err := strconv.ParseInt(busPort[1], 10, 0)
	if err != nil {
		return 0, fmt.Errorf("could not parse bus number %q: %w", busPort[1], err)
	}
	bus := int(b)

	p, err := strconv.ParseInt(busPort[2], 10, 0)
	if err != nil {
		return 0, fmt.Errorf("could not parse port number %q: %w", busPort[2], err)
	}
	port := int(p)

	if bus == m.usb2BusNumber {
		if port < 1 || port > m.usb2NumPorts {
			return 0, fmt.Errorf("invalid port number: %v", port)
		}
		return port, nil
	} else if bus == m.usb3BusNumber {
		if port < 1 || port > m.usb3NumPorts {
			return 0, fmt.Errorf("invalid port number: %v", port)
		}
		// USB 3.0 ports follow USB 2.0 ports.
		return port + m.usb2NumPorts, nil
	}

	return 0, fmt.Errorf("invalid bus number %v", bus)
}

func (s *tremplinServer) handleUsbUevent(uevent map[string]string) error {
	action, ok := uevent["ACTION"]
	if !ok {
		return nil
	}

	devName, ok := uevent["DEVNAME"]
	if !ok {
		return nil
	}

	devPath, ok := uevent["DEVPATH"]
	if !ok {
		return nil
	}

	m := s.usbManager
	prefix := fmt.Sprintf("/%s/usb", m.controllerPath)
	if !strings.HasPrefix(devPath, prefix) {
		// The device does not belong to the correct USB controller.
		return nil
	}

	port, err := m.getPortNumberFromDevPath(devPath)
	if err != nil {
		return fmt.Errorf("failed to get port number: %w", err)
	}

	m.Lock()
	defer m.Unlock()
	portInfo := m.getPortInfo(port)

	if portInfo.attached {
		container, etag, err := s.lxd.GetInstance(portInfo.containerName)
		if err != nil {
			return fmt.Errorf("failed to get container %q: %w", portInfo.containerName, err)
		}

		instancePut := container.Writable()

		if action == "add" {
			err = addDevice(devName, instancePut.Devices)
			if err != nil {
				return fmt.Errorf("failed to add device: %w", err)
			}
		} else if action == "remove" {
			err = removeDevice(devName, instancePut.Devices)
			if err != nil {
				return fmt.Errorf("failed to remove device: %w", err)
			}
		}

		op, err := s.lxd.UpdateInstance(portInfo.containerName, instancePut, etag)
		if err != nil {
			return fmt.Errorf("UpdateInstance failed %q: %w", portInfo.containerName, err)
		}
		if err := op.Wait(); err != nil {
			return fmt.Errorf("wait for UpdateInstance failed %q: %w", portInfo.containerName, err)
		}
	}

	// uevents relating to a USB port can be received before AttachUsbToContainer.
	// Record all known devices even if the USB port isn't attached yet, so
	// that they can be attached once AttachUsbToContainer is received.
	if action == "add" {
		portInfo.knownDevices[devName] = true
	} else if action == "remove" {
		delete(portInfo.knownDevices, devName)

		if len(portInfo.knownDevices) == 0 {
			portInfo.attached = false
			portInfo.containerName = ""
		}
	}

	return nil
}
