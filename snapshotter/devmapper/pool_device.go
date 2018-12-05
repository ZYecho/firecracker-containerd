// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package devmapper

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/hashicorp/go-multierror"
	"github.com/moby/moby/pkg/devicemapper"
	"github.com/pkg/errors"
)

const (
	maxDeviceID = 0xffffff // Device IDs are 24-bit numbers
)

// PoolDevice ties together data and metadata volumes, represents thin-pool and manages volumes, snapshots and device ids.
type PoolDevice struct {
	poolName        string
	currentDeviceID int
	devices         map[string]int
	mutex           sync.Mutex
}

// NewPoolDevice creates new thin-pool from existing data and metadata volumes.
func NewPoolDevice(ctx context.Context, poolName, dataVolume, metaVolume string, blockSizeSectors uint32) (*PoolDevice, error) {
	log.G(ctx).Infof("creating pool device '%s'", poolName)

	if driverVersion, err := devicemapper.GetDriverVersion(); err != nil {
		return nil, errors.Wrap(err, "failed to get driver version")
	} else {
		log.G(ctx).Debugf("using driver: %s", driverVersion)
	}

	if libVersion, err := devicemapper.GetLibraryVersion(); err != nil {
		return nil, errors.Wrap(err, "failed to get library version")
	} else {
		log.G(ctx).Debugf("using lib version: %s", libVersion)
	}

	log.G(ctx).Infof("creating pool (data: '%s', meta: '%s', block size: %d)", dataVolume, metaVolume, blockSizeSectors)

	dataFile, err := os.Open(dataVolume)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open data volume")
	}

	defer dataFile.Close()

	metaFile, err := os.Open(metaVolume)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open meta volume")
	}

	defer metaFile.Close()

	if err := devicemapper.CreatePool(poolName, dataFile, metaFile, blockSizeSectors); err != nil {
		return nil, errors.Wrapf(err, "failed to create thin-pool with name '%s'", poolName)
	}

	return &PoolDevice{
		poolName: poolName,
		devices:  make(map[string]int),
	}, nil
}

func (p *PoolDevice) CreateThinDevice(deviceName string, virtualSizeBytes uint64) (int, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if _, ok := p.devices[deviceName]; ok {
		return 0, errors.Errorf("device with name '%s' already created", deviceName)
	}

	// Create device, retry if device id is taken
	deviceID, err := p.tryAcquireDeviceID(func(thinDeviceID int) error {
		return devicemapper.CreateDevice(p.poolName, thinDeviceID)
	})

	if err != nil {
		return 0, errors.Wrap(err, "failed to create thin device")
	}

	p.devices[deviceName] = deviceID

	devicePath := p.GetDevicePath(p.poolName)
	if err := devicemapper.ActivateDevice(devicePath, deviceName, deviceID, virtualSizeBytes); err != nil {
		return 0, errors.Wrap(err, "failed to activate thin device")
	}

	return deviceID, nil
}

func (p *PoolDevice) CreateSnapshotDevice(deviceName string, snapshotName string, virtualSizeBytes uint64) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	deviceID, ok := p.devices[deviceName]
	if !ok {
		return errors.Errorf("device '%s' not found", deviceName)
	}

	_, ok = p.devices[snapshotName]
	if ok {
		return errors.Errorf("snapshot with name '%s' already exists", snapshotName)
	}

	// Send 'create_snap' message to pool-device
	devicePoolPath := p.GetDevicePath(p.poolName)
	thinDevicePath := p.GetDevicePath(deviceName)
	snapshotDeviceID, err := p.tryAcquireDeviceID(func(snapshotDeviceID int) error {
		return devicemapper.CreateSnapDevice(devicePoolPath, snapshotDeviceID, thinDevicePath, deviceID)
	})

	if err != nil {
		return errors.Wrap(err, "failed to create snapshot")
	}

	// Activate snapshot
	if err := devicemapper.ActivateDevice(devicePoolPath, snapshotName, snapshotDeviceID, virtualSizeBytes); err != nil {
		return errors.Wrap(err, "failed to activate snapshot device")
	}

	p.devices[snapshotName] = snapshotDeviceID
	return nil
}

func (p *PoolDevice) RemoveDevice(deviceName string) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	return p.removeDevice(deviceName)
}

func (p *PoolDevice) GetDevicePath(deviceName string) string {
	if strings.HasPrefix(deviceName, "/dev/mapper/") {
		return deviceName
	}

	return fmt.Sprintf("/dev/mapper/%s", deviceName)
}

func (p *PoolDevice) Close(ctx context.Context, removePool bool) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	var result *multierror.Error

	// Clean thin devices
	for name, id := range p.devices {
		if err := p.removeDevice(name); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to remove device '%s' (id: %d)", name, id)
			result = multierror.Append(result, err)
		}
	}

	if removePool {
		// Remove thin-pool
		if err := devicemapper.RemoveDevice(p.poolName); err != nil {
			log.G(ctx).WithError(err).Errorf("failed to remove thin-pool '%s'", p.poolName)
			result = multierror.Append(result, err)
		}
	}

	return result.ErrorOrNil()
}

func (p *PoolDevice) getNextDeviceID() int {
	p.currentDeviceID++
	if p.currentDeviceID >= maxDeviceID {
		p.currentDeviceID = 0
	}

	return p.currentDeviceID
}

func (p *PoolDevice) tryAcquireDeviceID(acquire func(deviceID int) error) (int, error) {
	attempt := 0

	for {
		deviceID := p.getNextDeviceID()
		err := acquire(deviceID)
		if err == nil {
			return deviceID, nil
		}

		if devicemapper.DeviceIDExists(err) {
			attempt++
			if attempt >= maxDeviceID {
				return 0, errors.Errorf("thin-pool error: all device ids are taken")
			}

			// This device ID already taken, try next one
			continue
		}

		// If errored for any other reason, just exit
		if err != nil {
			return 0, err
		}
	}
}

func (p *PoolDevice) removeDevice(name string) error {
	const (
		retryCount          = 3
		delayBetweenRetries = 500 * time.Millisecond
	)

	var (
		err        error
		devicePath = p.GetDevicePath(name)
	)

	for i := 0; i < retryCount; i++ {
		err = devicemapper.RemoveDevice(devicePath)
		if err == nil {
			delete(p.devices, name)
			return nil
		}

		if err == devicemapper.ErrBusy {
			time.Sleep(delayBetweenRetries)
			continue
		}

		return errors.Wrapf(err, "failed to remove device '%s'", name)
	}

	return errors.Wrapf(err, "failed to remove device '%s' after %d retries", name, retryCount)
}