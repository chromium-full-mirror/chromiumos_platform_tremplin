// Copyright 2022 The ChromiumOS Authors.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const (
	testUsbControllerPath = "devices/pci0000:00/0000:00:01.0"
	testUsb2BusNumber     = 1
	testUsb3BusNumber     = 2
	testUsb2NumPorts      = 8
	testUsb3NumPorts      = 8
)

func createUsbManagerForTesting() *containerUsbManager {
	m := new(containerUsbManager)
	m.controllerPath = testUsbControllerPath
	m.usb2BusNumber = testUsb2BusNumber
	m.usb3BusNumber = testUsb3BusNumber
	m.usb2NumPorts = testUsb2NumPorts
	m.usb3NumPorts = testUsb3NumPorts
	return m
}

func setupTestSysfs(sysfsDir string) error {
	controllerPath := fmt.Sprintf("%s/%s", sysfsDir, testUsbControllerPath)
	err := os.MkdirAll(controllerPath+"/usb1", 0755)
	if err != nil {
		return err
	}
	err = os.WriteFile(controllerPath+"/usb1/maxchild", []byte(fmt.Sprintf("%d\n", testUsb2NumPorts)), 0444)
	if err != nil {
		return err
	}
	err = os.WriteFile(controllerPath+"/usb1/idProduct", []byte("0002\n"), 0444)
	if err != nil {
		return err
	}

	err = os.MkdirAll(controllerPath+"/usb2", 0755)
	if err != nil {
		return err
	}
	err = os.WriteFile(controllerPath+"/usb2/maxchild", []byte(fmt.Sprintf("%d\n", testUsb3NumPorts)), 0444)
	if err != nil {
		return err
	}
	err = os.WriteFile(controllerPath+"/usb2/idProduct", []byte("0003\n"), 0444)
	if err != nil {
		return err
	}

	err = os.WriteFile(controllerPath+"/class", []byte("0x0c0330\n"), 0444)
	if err != nil {
		return err
	}

	err = os.MkdirAll(sysfsDir+"/bus/pci/devices", 0755)
	if err != nil {
		return err
	}
	err = os.Symlink("../../../"+testUsbControllerPath, fmt.Sprintf("%s/bus/pci/devices/%s", sysfsDir, filepath.Base(testUsbControllerPath)))
	if err != nil {
		return err
	}

	return nil
}

func TestInitUsb(t *testing.T) {
	testSysfsDir, err := os.MkdirTemp("", "sys")
	if err != nil {
		t.Fatalf("Failed to create tempdir: %v", err)
	}
	defer os.RemoveAll(testSysfsDir)

	err = setupTestSysfs(testSysfsDir)
	if err != nil {
		t.Fatalf("Failed to setup test sysfs: %v", err)
	}

	m := new(containerUsbManager)
	err = initUsb(m, testSysfsDir)
	if err != nil {
		t.Fatalf("initUsb failed: %v", err)
	}

	if !(m.controllerPath == testUsbControllerPath &&
		m.usb2BusNumber == testUsb2BusNumber &&
		m.usb3BusNumber == testUsb3BusNumber &&
		m.usb2NumPorts == testUsb2NumPorts &&
		m.usb3NumPorts == testUsb3NumPorts) {

		t.Fatal("Controller was not detected correctly")
	}
}

func TestGetPortNumberFromDevPath(t *testing.T) {
	m := createUsbManagerForTesting()

	// USB 2.0 root hub
	s := "/devices/pci0000:00/0000:00:01.0/usb1/1-1"
	portNum, err := m.getPortNumberFromDevPath(s)
	if err != nil {
		t.Fatal("getPortNumberFromDevPath failed")
	}
	if portNum != 1 {
		t.Fatalf("getPortNumberFromDevPath did not return 1 (got %d)", portNum)
	}

	// USB 3.0 root hub
	s = "/devices/pci0000:00/0000:00:01.0/usb2/2-1"
	portNum, err = m.getPortNumberFromDevPath(s)
	if err != nil {
		t.Fatal("getPortNumberFromDevPath failed")
	}
	if portNum != 9 {
		t.Fatalf("getPortNumberFromDevPath did not return 9 (got %d)", portNum)
	}

	// Extra parts are ignored
	s = "/devices/pci0000:00/0000:00:01.0/usb2/2-8/extra-parts"
	portNum, err = m.getPortNumberFromDevPath(s)
	if err != nil {
		t.Fatal("getPortNumberFromDevPath failed")
	}
	if portNum != 16 {
		t.Fatalf("getPortNumberFromDevPath did not return 16 (got %d)", portNum)
	}

	// Port too high
	s = "/devices/pci0000:00/0000:00:01.0/usb1/1-9"
	_, err = m.getPortNumberFromDevPath(s)
	if err == nil {
		t.Fatal("getPortNumberFromDevPath did not fail")
	}

	// Unknown root hub
	s = "/devices/pci0000:00/0000:00:01.0/usb3/3-1"
	_, err = m.getPortNumberFromDevPath(s)
	if err == nil {
		t.Fatal("getPortNumberFromDevPath did not fail")
	}

	// Missing parts
	s = "/devices/pci0000:00/0000:00:01.0/usb1/"
	_, err = m.getPortNumberFromDevPath(s)
	if err == nil {
		t.Fatal("getPortNumberFromDevPath did not fail")
	}

	// Not a port
	s = "/devices/pci0000:00/0000:00:01.0/usb1/1-0:1.0"
	_, err = m.getPortNumberFromDevPath(s)
	if err == nil {
		t.Fatal("getPortNumberFromDevPath did not fail")
	}
}
