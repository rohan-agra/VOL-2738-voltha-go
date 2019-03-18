/*
 * Copyright 2019-present Open Networking Foundation

 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at

 * http://www.apache.org/licenses/LICENSE-2.0

 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package core

import (
	"context"
	"fmt"
	"github.com/opencord/voltha-go/common/log"
	"github.com/opencord/voltha-go/db/kvstore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sync"
	"time"
)

type ownership struct {
	id    string
	owned bool
	chnl  chan int
}

type DeviceOwnership struct {
	instanceId         string
	exitChannel        chan int
	kvClient           kvstore.Client
	reservationTimeout int64 // Duration in seconds
	ownershipPrefix    string
	deviceMap          map[string]*ownership
	deviceMapLock      *sync.RWMutex
}

func NewDeviceOwnership(id string, kvClient kvstore.Client, ownershipPrefix string, reservationTimeout int64) *DeviceOwnership {
	var deviceOwnership DeviceOwnership
	deviceOwnership.instanceId = id
	deviceOwnership.exitChannel = make(chan int, 1)
	deviceOwnership.kvClient = kvClient
	deviceOwnership.ownershipPrefix = ownershipPrefix
	deviceOwnership.reservationTimeout = reservationTimeout
	deviceOwnership.deviceMap = make(map[string]*ownership)
	deviceOwnership.deviceMapLock = &sync.RWMutex{}
	return &deviceOwnership
}

func (da *DeviceOwnership) Start(ctx context.Context) {
	log.Info("starting-deviceOwnership", log.Fields{"instanceId": da.instanceId})
	log.Info("deviceOwnership-started")
}

func (da *DeviceOwnership) Stop(ctx context.Context) {
	log.Info("stopping-deviceOwnership")
	da.exitChannel <- 1
	// Need to flush all device reservations
	log.Info("deviceOwnership-stopped")
}

func (da *DeviceOwnership) tryToReserveKey(id string) bool {
	var currOwner string
	// Try to reserve the key
	kvKey := fmt.Sprintf("%s_%s", da.ownershipPrefix, id)
	value, err := da.kvClient.Reserve(kvKey, da.instanceId, da.reservationTimeout)
	if value != nil {
		if currOwner, err = kvstore.ToString(value); err != nil {
			log.Error("unexpected-owner-type")
		}
		return currOwner == da.instanceId
	}
	return false
}

func (da *DeviceOwnership) startOwnershipMonitoring(id string, chnl chan int) {
	var op string

startloop:
	for {
		da.deviceMapLock.RLock()
		val, exist := da.deviceMap[id]
		da.deviceMapLock.RUnlock()
		if exist && val.owned {
			// Device owned; renew reservation
			op = "renew"
			kvKey := fmt.Sprintf("%s_%s", da.ownershipPrefix, id)
			if err := da.kvClient.RenewReservation(kvKey); err != nil {
				log.Errorw("reservation-renewal-error", log.Fields{"error": err})
			}
		} else {
			// Device not owned; try to seize ownership
			op = "retry"
			if err := da.setOwnership(id, da.tryToReserveKey(id)); err != nil {
				log.Errorw("unexpected-error", log.Fields{"error": err})
			}
		}
		select {
		case <-da.exitChannel:
			log.Infow("closing-monitoring", log.Fields{"Id": id})
			break startloop
		case <-time.After(time.Duration(da.reservationTimeout) / 3 * time.Second):
			msg := fmt.Sprintf("%s-reservation", op)
			log.Infow(msg, log.Fields{"Id": id})
		case <-chnl:
			log.Infow("closing-device-monitoring", log.Fields{"Id": id})
			break startloop
		}
	}
}

func (da *DeviceOwnership) getOwnership(id string) bool {
	da.deviceMapLock.RLock()
	defer da.deviceMapLock.RUnlock()
	if val, exist := da.deviceMap[id]; exist {
		return val.owned
	}
	log.Debugw("setting-up-new-ownership", log.Fields{"Id": id})
	// Not owned by me or maybe anybody else.  Try to reserve it
	reservedByMe := da.tryToReserveKey(id)
	myChnl := make(chan int)
	da.deviceMap[id] = &ownership{id: id, owned: reservedByMe, chnl: myChnl}
	go da.startOwnershipMonitoring(id, myChnl)
	return reservedByMe
}

func (da *DeviceOwnership) setOwnership(id string, owner bool) error {
	da.deviceMapLock.Lock()
	defer da.deviceMapLock.Unlock()
	if _, exist := da.deviceMap[id]; exist {
		if da.deviceMap[id].owned != owner {
			log.Debugw("ownership-changed", log.Fields{"Id": id, "owner": owner})
		}
		da.deviceMap[id].owned = owner
		return nil
	}
	return status.Error(codes.NotFound, fmt.Sprintf("id-inexistent-%s", id))
}

// OwnedByMe returns where this Core instance active owns this device.   This function will automatically
// trigger the process to monitor the device and update the device ownership regularly.
func (da *DeviceOwnership) OwnedByMe(id string) bool {
	return da.getOwnership(id)
}

//AbandonDevice must be invoked whenever a device is deleted from the Core
func (da *DeviceOwnership) AbandonDevice(id string) error {
	da.deviceMapLock.Lock()
	defer da.deviceMapLock.Unlock()
	if o, exist := da.deviceMap[id]; exist {
		// Stop the Go routine monitoring the device
		close(o.chnl)
		delete(da.deviceMap, id)
		log.Debugw("abandoning-device", log.Fields{"Id": id})
		return nil
	}
	return status.Error(codes.NotFound, fmt.Sprintf("id-inexistent-%s", id))
}
