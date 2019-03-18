/*
 * Copyright 2018-present Open Networking Foundation

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
	"errors"
	"github.com/opencord/voltha-go/common/log"
	"github.com/opencord/voltha-go/db/model"
	"github.com/opencord/voltha-go/kafka"
	ic "github.com/opencord/voltha-go/protos/inter_container"
	ofp "github.com/opencord/voltha-go/protos/openflow_13"
	"github.com/opencord/voltha-go/protos/voltha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"reflect"
	"runtime"
	"sync"
)

type DeviceManager struct {
	deviceAgents        map[string]*DeviceAgent
	core                *Core
	adapterProxy        *AdapterProxy
	adapterMgr          *AdapterManager
	logicalDeviceMgr    *LogicalDeviceManager
	kafkaICProxy        *kafka.InterContainerProxy
	stateTransitions    *TransitionMap
	clusterDataProxy    *model.Proxy
	coreInstanceId      string
	exitChannel         chan int
	lockDeviceAgentsMap sync.RWMutex
}

func newDeviceManager(core *Core) *DeviceManager {
	var deviceMgr DeviceManager
	deviceMgr.core = core
	deviceMgr.exitChannel = make(chan int, 1)
	deviceMgr.deviceAgents = make(map[string]*DeviceAgent)
	deviceMgr.kafkaICProxy = core.kmp
	deviceMgr.adapterProxy = NewAdapterProxy(core.kmp)
	deviceMgr.coreInstanceId = core.instanceId
	deviceMgr.clusterDataProxy = core.clusterDataProxy
	deviceMgr.adapterMgr = core.adapterMgr
	deviceMgr.lockDeviceAgentsMap = sync.RWMutex{}
	return &deviceMgr
}

func (dMgr *DeviceManager) start(ctx context.Context, logicalDeviceMgr *LogicalDeviceManager) {
	log.Info("starting-device-manager")
	dMgr.logicalDeviceMgr = logicalDeviceMgr
	dMgr.stateTransitions = NewTransitionMap(dMgr)
	log.Info("device-manager-started")
}

func (dMgr *DeviceManager) stop(ctx context.Context) {
	log.Info("stopping-device-manager")
	dMgr.exitChannel <- 1
	log.Info("device-manager-stopped")
}

func sendResponse(ctx context.Context, ch chan interface{}, result interface{}) {
	if ctx.Err() == nil {
		// Returned response only of the ctx has not been cancelled/timeout/etc
		// Channel is automatically closed when a context is Done
		ch <- result
		log.Debugw("sendResponse", log.Fields{"result": result})
	} else {
		// Should the transaction be reverted back?
		log.Debugw("sendResponse-context-error", log.Fields{"context-error": ctx.Err()})
	}
}

func (dMgr *DeviceManager) addDeviceAgentToMap(agent *DeviceAgent) {
	dMgr.lockDeviceAgentsMap.Lock()
	defer dMgr.lockDeviceAgentsMap.Unlock()
	if _, exist := dMgr.deviceAgents[agent.deviceId]; !exist {
		dMgr.deviceAgents[agent.deviceId] = agent
	}
}

func (dMgr *DeviceManager) deleteDeviceAgentToMap(agent *DeviceAgent) {
	dMgr.lockDeviceAgentsMap.Lock()
	defer dMgr.lockDeviceAgentsMap.Unlock()
	delete(dMgr.deviceAgents, agent.deviceId)
}

// getDeviceAgent returns the agent managing the device.  If the device is not in memory, it will loads it, if it exists
func (dMgr *DeviceManager) getDeviceAgent(deviceId string) *DeviceAgent {
	dMgr.lockDeviceAgentsMap.Lock()
	if agent, ok := dMgr.deviceAgents[deviceId]; ok {
		dMgr.lockDeviceAgentsMap.Unlock()
		return agent
	} else {
		//	Try to load into memory - loading will also create the device agent
		dMgr.lockDeviceAgentsMap.Unlock()
		if err := dMgr.load(deviceId); err == nil {
			dMgr.lockDeviceAgentsMap.Lock()
			defer dMgr.lockDeviceAgentsMap.Unlock()
			if agent, ok = dMgr.deviceAgents[deviceId]; ok {
				return agent
			}
		}
	}
	return nil
}

// listDeviceIdsFromMap returns the list of device IDs that are in memory
func (dMgr *DeviceManager) listDeviceIdsFromMap() *voltha.IDs {
	dMgr.lockDeviceAgentsMap.Lock()
	defer dMgr.lockDeviceAgentsMap.Unlock()
	result := &voltha.IDs{Items: make([]*voltha.ID, 0)}
	for key, _ := range dMgr.deviceAgents {
		result.Items = append(result.Items, &voltha.ID{Id: key})
	}
	return result
}

func (dMgr *DeviceManager) createDevice(ctx context.Context, device *voltha.Device, ch chan interface{}) {
	log.Debugw("createDevice", log.Fields{"device": device, "aproxy": dMgr.adapterProxy})

	// Ensure this device is set as root
	device.Root = true
	// Create and start a device agent for that device
	agent := newDeviceAgent(dMgr.adapterProxy, device, dMgr, dMgr.clusterDataProxy)
	dMgr.addDeviceAgentToMap(agent)
	agent.start(ctx, false)

	sendResponse(ctx, ch, agent.lastData)
}

func (dMgr *DeviceManager) enableDevice(ctx context.Context, id *voltha.ID, ch chan interface{}) {
	log.Debugw("enableDevice", log.Fields{"deviceid": id})
	var res interface{}
	if agent := dMgr.getDeviceAgent(id.Id); agent != nil {
		res = agent.enableDevice(ctx)
		log.Debugw("EnableDevice-result", log.Fields{"result": res})
	}

	sendResponse(ctx, ch, res)
}

func (dMgr *DeviceManager) disableDevice(ctx context.Context, id *voltha.ID, ch chan interface{}) {
	log.Debugw("disableDevice", log.Fields{"deviceid": id})
	var res interface{}
	if agent := dMgr.getDeviceAgent(id.Id); agent != nil {
		res = agent.disableDevice(ctx)
		log.Debugw("disableDevice-result", log.Fields{"result": res})
	} else {
		res = status.Errorf(codes.NotFound, "%s", id.Id)
	}

	sendResponse(ctx, ch, res)
}

func (dMgr *DeviceManager) rebootDevice(ctx context.Context, id *voltha.ID, ch chan interface{}) {
	log.Debugw("rebootDevice", log.Fields{"deviceid": id})
	var res interface{}
	if agent := dMgr.getDeviceAgent(id.Id); agent != nil {
		res = agent.rebootDevice(ctx)
		log.Debugw("rebootDevice-result", log.Fields{"result": res})
	} else {
		res = status.Errorf(codes.NotFound, "%s", id.Id)
	}
	sendResponse(ctx, ch, res)
}

func (dMgr *DeviceManager) deleteDevice(ctx context.Context, id *voltha.ID, ch chan interface{}) {
	log.Debugw("deleteDevice", log.Fields{"deviceid": id})
	var res interface{}
	if agent := dMgr.getDeviceAgent(id.Id); agent != nil {
		res = agent.deleteDevice(ctx)
		if res == nil { //Success
			agent.stop(ctx)
			dMgr.deleteDeviceAgentToMap(agent)
			dMgr.core.deviceOwnership.AbandonDevice(id.Id)
		}
		log.Debugw("deleteDevice-result", log.Fields{"result": res})
	} else {
		res = status.Errorf(codes.NotFound, "%s", id.Id)
	}
	sendResponse(ctx, ch, res)
}

// GetDevice will returns a device, either from memory or from the dB, if present
func (dMgr *DeviceManager) GetDevice(id string) (*voltha.Device, error) {
	log.Debugw("GetDevice", log.Fields{"deviceid": id})
	if agent := dMgr.getDeviceAgent(id); agent != nil {
		return agent.getDevice()
	}
	return nil, status.Errorf(codes.NotFound, "%s", id)
}

func (dMgr *DeviceManager) GetChildDevice(parentDeviceId string, serialNumber string, onuId int64, parentPortNo int64) (*voltha.Device, error) {
	log.Debugw("GetChildDevice", log.Fields{"parentDeviceid": parentDeviceId, "serialNumber": serialNumber})

	var parentDevice *voltha.Device
	var err error
	if parentDevice, err = dMgr.GetDevice(parentDeviceId); err != nil {
		return nil, status.Errorf(codes.Aborted, "%s", err.Error())
	}
	var childDeviceIds []string
	if childDeviceIds, err = dMgr.getAllChildDeviceIds(parentDevice); err != nil {
		return nil, status.Errorf(codes.Aborted, "%s", err.Error())
	}
	if len(childDeviceIds) == 0 {
		log.Debugw("no-child-devices", log.Fields{"parentDeviceId": parentDevice.Id})
		return nil, status.Errorf(codes.NotFound, "%s", parentDeviceId)
	}

	var foundChildDevice *voltha.Device
	for _, childDeviceId := range childDeviceIds {
		found := false
		if searchDevice, err := dMgr.GetDevice(childDeviceId); err == nil {

			foundOnuId := false
			if searchDevice.ProxyAddress.OnuId == uint32(onuId) {
				if searchDevice.ParentPortNo == uint32(parentPortNo) {
					log.Debugw("found-child-by-onuid", log.Fields{"parentDeviceId": parentDevice.Id, "onuId": onuId})
					foundOnuId = true
				}
			}

			foundSerialNumber := false
			if searchDevice.SerialNumber == serialNumber {
				log.Debugw("found-child-by-serialnumber", log.Fields{"parentDeviceId": parentDevice.Id, "serialNumber": serialNumber})
				foundSerialNumber = true
			}

			// if both onuId and serialNumber are provided both must be true for the device to be found
			// otherwise whichever one found a match is good enough
			if onuId > 0 && serialNumber != "" {
				found = foundOnuId && foundSerialNumber
			} else {
				found = foundOnuId || foundSerialNumber
			}

			if found == true {
				foundChildDevice = searchDevice
				break
			}
		}
	}

	if foundChildDevice != nil {
		log.Debugw("child-device-found", log.Fields{"parentDeviceId": parentDevice.Id, "foundChildDevice": foundChildDevice})
		return foundChildDevice, nil
	}

	log.Warnw("child-device-not-found", log.Fields{"parentDeviceId": parentDevice.Id,
		"serialNumber": serialNumber, "onuId": onuId, "parentPortNo": parentPortNo})
	return nil, status.Errorf(codes.NotFound, "%s", parentDeviceId)
}

func (dMgr *DeviceManager) GetChildDeviceWithProxyAddress(proxyAddress *voltha.Device_ProxyAddress) (*voltha.Device, error) {
	log.Debugw("GetChildDeviceWithProxyAddress", log.Fields{"proxyAddress": proxyAddress})

	var parentDevice *voltha.Device
	var err error
	if parentDevice, err = dMgr.GetDevice(proxyAddress.DeviceId); err != nil {
		return nil, status.Errorf(codes.Aborted, "%s", err.Error())
	}
	var childDeviceIds []string
	if childDeviceIds, err = dMgr.getAllChildDeviceIds(parentDevice); err != nil {
		return nil, status.Errorf(codes.Aborted, "%s", err.Error())
	}
	if len(childDeviceIds) == 0 {
		log.Debugw("no-child-devices", log.Fields{"parentDeviceId": parentDevice.Id})
		return nil, status.Errorf(codes.NotFound, "%s", proxyAddress)
	}

	var foundChildDevice *voltha.Device
	for _, childDeviceId := range childDeviceIds {
		if searchDevice, err := dMgr.GetDevice(childDeviceId); err == nil {
			if searchDevice.ProxyAddress == proxyAddress {
				foundChildDevice = searchDevice
				break
			}
		}
	}

	if foundChildDevice != nil {
		log.Debugw("child-device-found", log.Fields{"proxyAddress": proxyAddress})
		return foundChildDevice, nil
	}

	log.Warnw("child-device-not-found", log.Fields{"proxyAddress": proxyAddress})
	return nil, status.Errorf(codes.NotFound, "%s", proxyAddress)
}

func (dMgr *DeviceManager) IsDeviceInCache(id string) bool {
	dMgr.lockDeviceAgentsMap.Lock()
	defer dMgr.lockDeviceAgentsMap.Unlock()
	_, exist := dMgr.deviceAgents[id]
	return exist
}

func (dMgr *DeviceManager) IsRootDevice(id string) (bool, error) {
	device, err := dMgr.GetDevice(id)
	if err != nil {
		return false, err
	}
	return device.Root, nil
}

// ListDevices retrieves the latest devices from the data model
func (dMgr *DeviceManager) ListDevices() (*voltha.Devices, error) {
	log.Debug("ListDevices")
	result := &voltha.Devices{}
	if devices := dMgr.clusterDataProxy.List("/devices", 0, false, ""); devices != nil {
		for _, device := range devices.([]interface{}) {
			// If device is not in memory then set it up
			if !dMgr.IsDeviceInCache(device.(*voltha.Device).Id) {
				agent := newDeviceAgent(dMgr.adapterProxy, device.(*voltha.Device), dMgr, dMgr.clusterDataProxy)
				if err := agent.start(nil, true); err != nil {
					log.Warnw("failure-starting-agent", log.Fields{"deviceId": device.(*voltha.Device).Id})
					agent.stop(nil)
				} else {
					dMgr.addDeviceAgentToMap(agent)
				}
			}
			result.Items = append(result.Items, device.(*voltha.Device))
		}
	}
	return result, nil
}

// loadDevice loads the deviceId in memory, if not present
func (dMgr *DeviceManager) loadDevice(deviceId string) (*DeviceAgent, error) {
	log.Debugw("loading-device", log.Fields{"deviceId": deviceId})
	// Sanity check
	if deviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "deviceId empty")
	}
	if !dMgr.IsDeviceInCache(deviceId) {
		agent := newDeviceAgent(dMgr.adapterProxy, &voltha.Device{Id: deviceId}, dMgr, dMgr.clusterDataProxy)
		if err := agent.start(nil, true); err != nil {
			agent.stop(nil)
			return nil, err
		}
		dMgr.addDeviceAgentToMap(agent)
	}
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent, nil
	}
	return nil, status.Error(codes.NotFound, deviceId) // This should not happen
}

// loadRootDeviceParentAndChildren loads the children and parents of a root device in memory
func (dMgr *DeviceManager) loadRootDeviceParentAndChildren(device *voltha.Device) error {
	log.Debugw("loading-parent-and-children", log.Fields{"deviceId": device.Id})
	if device.Root {
		// Scenario A
		if device.ParentId != "" {
			//	 Load logical device if needed.
			if err := dMgr.logicalDeviceMgr.load(device.ParentId); err != nil {
				log.Warnw("failure-loading-logical-device", log.Fields{"lDeviceId": device.ParentId})
			}
		} else {
			log.Debugw("no-parent-to-load", log.Fields{"deviceId": device.Id})
		}
		//	Load all child devices, if needed
		if childDeviceIds, err := dMgr.getAllChildDeviceIds(device); err == nil {
			for _, childDeviceId := range childDeviceIds {
				if _, err := dMgr.loadDevice(childDeviceId); err != nil {
					log.Warnw("failure-loading-device", log.Fields{"deviceId": childDeviceId})
					return err
				}
			}
			log.Debugw("loaded-children", log.Fields{"deviceId": device.Id, "numChildren": len(childDeviceIds)})
		} else {
			log.Debugw("no-child-to-load", log.Fields{"deviceId": device.Id})
		}
	}
	return nil
}

// load loads the deviceId in memory, if not present, and also loads its accompanying parents and children.  Loading
// in memory is for improved performance.  It is not imperative that a device needs to be in memory when a request
// acting on the device is received by the core. In such a scenario, the Core will load the device in memory first
// and the proceed with the request.
func (dMgr *DeviceManager) load(deviceId string) error {
	log.Debug("load...")
	// First load the device - this may fail in case the device was deleted intentionally by the other core
	var dAgent *DeviceAgent
	var err error
	if dAgent, err = dMgr.loadDevice(deviceId); err != nil {
		log.Warnw("failure-loading-device", log.Fields{"deviceId": deviceId})
		return err
	}
	// Get the loaded device details
	var device *voltha.Device
	if device, err = dAgent.getDevice(); err != nil {
		return err
	}

	// If the device is in Pre-provisioning or deleted state stop here
	if device.AdminState == voltha.AdminState_PREPROVISIONED || device.AdminState == voltha.AdminState_DELETED {
		return nil
	}

	// Now we face two scenarios
	if device.Root {
		// Load all children as well as the parent of this device (logical_device)
		if err := dMgr.loadRootDeviceParentAndChildren(device); err != nil {
			log.Warnw("failure-loading-device-parent-and-children", log.Fields{"deviceId": deviceId})
			return err
		}
		log.Debugw("successfully-loaded-parent-and-children", log.Fields{"deviceId": deviceId})
	} else {
		//	Scenario B - use the parentId of that device (root device) to trigger the loading
		if device.ParentId != "" {
			return dMgr.load(device.ParentId)
		}
	}
	return nil
}

// ListDeviceIds retrieves the latest device IDs information from the data model (memory data only)
func (dMgr *DeviceManager) ListDeviceIds() (*voltha.IDs, error) {
	log.Debug("ListDeviceIDs")
	// Report only device IDs that are in the device agent map
	return dMgr.listDeviceIdsFromMap(), nil
}

//ReconcileDevices is a request to a voltha core to managed a list of devices based on their IDs
func (dMgr *DeviceManager) ReconcileDevices(ctx context.Context, ids *voltha.IDs, ch chan interface{}) {
	log.Debug("ReconcileDevices")
	var res interface{}
	if ids != nil {
		toReconcile := len(ids.Items)
		reconciled := 0
		for _, id := range ids.Items {
			//	 Act on the device only if its not present in the agent map
			if !dMgr.IsDeviceInCache(id.Id) {
				//	Device Id not in memory
				log.Debugw("reconciling-device", log.Fields{"id": id.Id})
				// Load device from dB
				agent := newDeviceAgent(dMgr.adapterProxy, &voltha.Device{Id: id.Id}, dMgr, dMgr.clusterDataProxy)
				if err := agent.start(nil, true); err != nil {
					log.Warnw("failure-loading-device", log.Fields{"deviceId": id.Id})
					agent.stop(nil)
				} else {
					dMgr.addDeviceAgentToMap(agent)
					reconciled += 1
				}
			} else {
				reconciled += 1
			}
		}
		if toReconcile != reconciled {
			res = status.Errorf(codes.DataLoss, "less-device-reconciled:%d/%d", reconciled, toReconcile)
		}
	} else {
		res = status.Errorf(codes.InvalidArgument, "empty-list-of-ids")
	}
	sendResponse(ctx, ch, res)
}

func (dMgr *DeviceManager) updateDevice(device *voltha.Device) error {
	log.Debugw("updateDevice", log.Fields{"deviceid": device.Id, "device": device})
	if agent := dMgr.getDeviceAgent(device.Id); agent != nil {
		return agent.updateDevice(device)
	}
	return status.Errorf(codes.NotFound, "%s", device.Id)
}

func (dMgr *DeviceManager) addPort(deviceId string, port *voltha.Port) error {
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		if err := agent.addPort(port); err != nil {
			return err
		}
		//	Setup peer ports
		meAsPeer := &voltha.Port_PeerPort{DeviceId: deviceId, PortNo: port.PortNo}
		for _, peerPort := range port.Peers {
			if agent := dMgr.getDeviceAgent(peerPort.DeviceId); agent != nil {
				if err := agent.addPeerPort(meAsPeer); err != nil {
					log.Errorw("failed-to-add-peer", log.Fields{"peer-device-id": peerPort.DeviceId})
					return err
				}
			}
		}
		// Notify the logical device manager to setup a logical port if needed
		if port.Type == voltha.Port_ETHERNET_NNI || port.Type == voltha.Port_ETHERNET_UNI {
			if device , err := dMgr.GetDevice(deviceId); err == nil {
				go dMgr.logicalDeviceMgr.addLogicalPort(device, port)
			} else {
				log.Errorw("failed-to-retrieve-device", log.Fields{"deviceId": deviceId})
				return err
			}
		}
		return nil
	} else {
		return status.Errorf(codes.NotFound, "%s", deviceId)
	}
}

func (dMgr *DeviceManager) updateFlows(deviceId string, flows []*ofp.OfpFlowStats) error {
	log.Debugw("updateFlows", log.Fields{"deviceid": deviceId})
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.updateFlows(flows)
	}
	return status.Errorf(codes.NotFound, "%s", deviceId)
}

func (dMgr *DeviceManager) updateGroups(deviceId string, groups []*ofp.OfpGroupEntry) error {
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.updateGroups(groups)
	}
	return status.Errorf(codes.NotFound, "%s", deviceId)
}

func (dMgr *DeviceManager) updatePmConfigs(deviceId string, pmConfigs *voltha.PmConfigs) error {
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.updatePmConfigs(pmConfigs)
	}
	return status.Errorf(codes.NotFound, "%s", deviceId)
}

func (dMgr *DeviceManager) getSwitchCapability(ctx context.Context, deviceId string) (*ic.SwitchCapability, error) {
	log.Debugw("getSwitchCapability", log.Fields{"deviceid": deviceId})
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.getSwitchCapability(ctx)
	}
	return nil, status.Errorf(codes.NotFound, "%s", deviceId)
}

func (dMgr *DeviceManager) getPorts(ctx context.Context, deviceId string, portType voltha.Port_PortType) (*voltha.Ports, error) {
	log.Debugw("getPorts", log.Fields{"deviceid": deviceId, "portType": portType})
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.getPorts(ctx, portType), nil
	}
	return nil, status.Errorf(codes.NotFound, "%s", deviceId)

}

func (dMgr *DeviceManager) getPortCapability(ctx context.Context, deviceId string, portNo uint32) (*ic.PortCapability, error) {
	log.Debugw("getPortCapability", log.Fields{"deviceid": deviceId})
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.getPortCapability(ctx, portNo)
	}
	return nil, status.Errorf(codes.NotFound, "%s", deviceId)
}

func (dMgr *DeviceManager) updateDeviceStatus(deviceId string, operStatus voltha.OperStatus_OperStatus, connStatus voltha.ConnectStatus_ConnectStatus) error {
	log.Debugw("updateDeviceStatus", log.Fields{"deviceid": deviceId, "operStatus": operStatus, "connStatus": connStatus})
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.updateDeviceStatus(operStatus, connStatus)
	}
	return status.Errorf(codes.NotFound, "%s", deviceId)
}

func (dMgr *DeviceManager) updateChildrenStatus(deviceId string, operStatus voltha.OperStatus_OperStatus, connStatus voltha.ConnectStatus_ConnectStatus) error {
	log.Debugw("updateChildrenStatus", log.Fields{"parentDeviceid": deviceId, "operStatus": operStatus, "connStatus": connStatus})
	var parentDevice *voltha.Device
	var err error
	if parentDevice, err = dMgr.GetDevice(deviceId); err != nil {
		return status.Errorf(codes.Aborted, "%s", err.Error())
	}
	var childDeviceIds []string
	if childDeviceIds, err = dMgr.getAllChildDeviceIds(parentDevice); err != nil {
		return status.Errorf(codes.Aborted, "%s", err.Error())
	}
	if len(childDeviceIds) == 0 {
		log.Debugw("no-child-device", log.Fields{"parentDeviceId": parentDevice.Id})
	}
	for _, childDeviceId := range childDeviceIds {
		if agent := dMgr.getDeviceAgent(childDeviceId); agent != nil {
			if err = agent.updateDeviceStatus(operStatus, connStatus); err != nil {
				return status.Errorf(codes.Aborted, "childDevice:%s, error:%s", childDeviceId, err.Error())
			}
		}
	}
	return nil
}

func (dMgr *DeviceManager) updatePortState(deviceId string, portType voltha.Port_PortType, portNo uint32, operStatus voltha.OperStatus_OperStatus) error {
	log.Debugw("updatePortState", log.Fields{"deviceid": deviceId, "portType": portType, "portNo": portNo, "operStatus": operStatus})
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.updatePortState(portType, portNo, operStatus)
	}
	return status.Errorf(codes.NotFound, "%s", deviceId)
}

func (dMgr *DeviceManager) childDeviceDetected(parentDeviceId string, parentPortNo int64, deviceType string,
	channelId int64, vendorId string, serialNumber string, onuId int64) error {
	log.Debugw("childDeviceDetected", log.Fields{"parentDeviceId": parentDeviceId})

	// Create the ONU device
	childDevice := &voltha.Device{}
	childDevice.Type = deviceType
	childDevice.ParentId = parentDeviceId
	childDevice.ParentPortNo = uint32(parentPortNo)
	childDevice.VendorId = vendorId
	childDevice.SerialNumber = serialNumber
	childDevice.Root = false

	//Get parent device type
	parent, err := dMgr.GetDevice(parentDeviceId)
	if err != nil {
		log.Error("no-parent-found", log.Fields{"parentId": parentDeviceId})
		return status.Errorf(codes.NotFound, "%s", parentDeviceId)
	}

	if _, err := dMgr.GetChildDevice(parentDeviceId, serialNumber, onuId, parentPortNo); err == nil {
		log.Warnw("child-device-exists", log.Fields{"parentId": parentDeviceId, "serialNumber": serialNumber})
		return status.Errorf(codes.AlreadyExists, "%s", serialNumber)
	}

	childDevice.ProxyAddress = &voltha.Device_ProxyAddress{DeviceId: parentDeviceId, DeviceType: parent.Type, ChannelId: uint32(channelId), OnuId: uint32(onuId)}

	// Create and start a device agent for that device
	agent := newDeviceAgent(dMgr.adapterProxy, childDevice, dMgr, dMgr.clusterDataProxy)
	dMgr.addDeviceAgentToMap(agent)
	agent.start(nil, false)

	// Set device ownership
	dMgr.core.deviceOwnership.OwnedByMe(agent.deviceId)

	// Activate the child device
	if agent := dMgr.getDeviceAgent(agent.deviceId); agent != nil {
		go agent.enableDevice(nil)
	}

	// Publish on the messaging bus that we have discovered new devices
	go dMgr.kafkaICProxy.DeviceDiscovered(agent.deviceId, deviceType, parentDeviceId, dMgr.coreInstanceId)

	return nil
}

func (dMgr *DeviceManager) processTransition(previous *voltha.Device, current *voltha.Device) error {
	// This will be triggered on every update to the device.
	handlers := dMgr.stateTransitions.GetTransitionHandler(previous, current)
	if handlers == nil {
		log.Debugw("no-op-transition", log.Fields{"deviceId": current.Id})
		return nil
	}
	for _, handler := range handlers {
		log.Debugw("running-handler", log.Fields{"handler": funcName(handler)})
		if err := handler(current); err != nil {
			return err
		}
	}
	return nil
}

func (dMgr *DeviceManager) packetOut(deviceId string, outPort uint32, packet *ofp.OfpPacketOut) error {
	log.Debugw("packetOut", log.Fields{"deviceId": deviceId, "outPort": outPort})
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.packetOut(outPort, packet)
	}
	return status.Errorf(codes.NotFound, "%s", deviceId)
}

func (dMgr *DeviceManager) PacketIn(deviceId string, port uint32, transactionId string, packet []byte) error {
	log.Debugw("PacketIn", log.Fields{"deviceId": deviceId, "port": port})
	// Get the logical device Id based on the deviceId
	var device *voltha.Device
	var err error
	if device, err = dMgr.GetDevice(deviceId); err != nil {
		log.Errorw("device-not-found", log.Fields{"deviceId": deviceId})
		return err
	}
	if !device.Root {
		log.Errorw("device-not-root", log.Fields{"deviceId": deviceId})
		return status.Errorf(codes.FailedPrecondition, "%s", deviceId)
	}

	if err := dMgr.logicalDeviceMgr.packetIn(device.ParentId, port, transactionId, packet); err != nil {
		return err
	}
	return nil
}

func (dMgr *DeviceManager) createLogicalDevice(cDevice *voltha.Device) error {
	log.Info("createLogicalDevice")
	var logicalId *string
	var err error
	if logicalId, err = dMgr.logicalDeviceMgr.createLogicalDevice(nil, cDevice); err != nil {
		log.Warnw("createlogical-device-error", log.Fields{"device": cDevice})
		return err
	}
	// Update the parent device with the logical id
	dMgr.UpdateDeviceAttribute(cDevice.Id, "ParentId", *logicalId)
	return nil
}

func (dMgr *DeviceManager) deleteLogicalDevice(cDevice *voltha.Device) error {
	log.Info("deleteLogicalDevice")
	var err error
	if err = dMgr.logicalDeviceMgr.deleteLogicalDevice(nil, cDevice); err != nil {
		log.Warnw("deleteLogical-device-error", log.Fields{"deviceId": cDevice.Id})
		return err
	}
	// Remove the logical device Id from the parent device
	logicalId := ""
	dMgr.UpdateDeviceAttribute(cDevice.Id, "ParentId", logicalId)
	return nil
}

func (dMgr *DeviceManager) deleteLogicalPort(device *voltha.Device) error {
	log.Info("deleteLogicalPort")
	var err error
	// Get the logical port associated with this device
	var lPortId *voltha.LogicalPortId
	if lPortId, err = dMgr.logicalDeviceMgr.getLogicalPortId(device); err != nil {
		log.Warnw("getLogical-port-error", log.Fields{"deviceId": device.Id, "error": err})
		return err
	}
	if err = dMgr.logicalDeviceMgr.deleteLogicalPort(nil, lPortId); err != nil {
		log.Warnw("deleteLogical-port-error", log.Fields{"deviceId": device.Id})
		return err
	}
	return nil
}

func (dMgr *DeviceManager) getParentDevice(childDevice *voltha.Device) *voltha.Device {
	//	Sanity check
	if childDevice.Root {
		// childDevice is the parent device
		return childDevice
	}
	parentDevice, _ := dMgr.GetDevice(childDevice.ParentId)
	return parentDevice
}

/*
All the functions below are callback functions where they are invoked with the latest and previous data.  We can
therefore use the data as is without trying to get the latest from the model.
*/

//disableAllChildDevices is invoked as a callback when the parent device is disabled
func (dMgr *DeviceManager) disableAllChildDevices(parentDevice *voltha.Device) error {
	log.Debug("disableAllChildDevices")
	var childDeviceIds []string
	var err error
	if childDeviceIds, err = dMgr.getAllChildDeviceIds(parentDevice); err != nil {
		return status.Errorf(codes.NotFound, "%s", parentDevice.Id)
	}
	if len(childDeviceIds) == 0 {
		log.Debugw("no-child-device", log.Fields{"parentDeviceId": parentDevice.Id})
	}
	allChildDisable := true
	for _, childDeviceId := range childDeviceIds {
		if agent := dMgr.getDeviceAgent(childDeviceId); agent != nil {
			if err = agent.disableDevice(nil); err != nil {
				log.Errorw("failure-disable-device", log.Fields{"deviceId": childDeviceId, "error": err.Error()})
				allChildDisable = false
			}
		}
	}
	if !allChildDisable {
		return err
	}
	return nil
}

//deleteAllChildDevices is invoked as a callback when the parent device is deleted
func (dMgr *DeviceManager) deleteAllChildDevices(parentDevice *voltha.Device) error {
	log.Debug("deleteAllChildDevices")
	var childDeviceIds []string
	var err error
	if childDeviceIds, err = dMgr.getAllChildDeviceIds(parentDevice); err != nil {
		return status.Errorf(codes.NotFound, "%s", parentDevice.Id)
	}
	if len(childDeviceIds) == 0 {
		log.Debugw("no-child-device", log.Fields{"parentDeviceId": parentDevice.Id})
	}
	allChildDeleted := true
	for _, childDeviceId := range childDeviceIds {
		if agent := dMgr.getDeviceAgent(childDeviceId); agent != nil {
			if err = agent.deleteDevice(nil); err != nil {
				log.Errorw("failure-delete-device", log.Fields{"deviceId": childDeviceId, "error": err.Error()})
				allChildDeleted = false
			} else {
				agent.stop(nil)
				dMgr.deleteDeviceAgentToMap(agent)
			}
		}
	}
	if !allChildDeleted {
		return err
	}
	return nil
}

//getAllChildDeviceIds is a helper method to get all the child device IDs from the device passed as parameter
func (dMgr *DeviceManager) getAllChildDeviceIds(parentDevice *voltha.Device) ([]string, error) {
	log.Debugw("getAllChildDeviceIds", log.Fields{"parentDeviceId": parentDevice.Id})
	childDeviceIds := make([]string, 0)
	if parentDevice != nil {
		for _, port := range parentDevice.Ports {
			for _, peer := range port.Peers {
				childDeviceIds = append(childDeviceIds, peer.DeviceId)
			}
		}
	}
	log.Debugw("returning-getAllChildDeviceIds", log.Fields{"parentDeviceId": parentDevice.Id, "childDeviceIds": childDeviceIds})
	return childDeviceIds, nil
}

//getAllChildDevices is a helper method to get all the child device IDs from the device passed as parameter
func (dMgr *DeviceManager) getAllChildDevices(parentDeviceId string) (*voltha.Devices, error) {
	log.Debugw("getAllChildDevices", log.Fields{"parentDeviceId": parentDeviceId})
	if parentDevice, err := dMgr.GetDevice(parentDeviceId); err == nil {
		childDevices := make([]*voltha.Device, 0)
		if childDeviceIds, er := dMgr.getAllChildDeviceIds(parentDevice); er == nil {
			for _, deviceId := range childDeviceIds {
				if d, e := dMgr.GetDevice(deviceId); e == nil && d != nil {
					childDevices = append(childDevices, d)
				}
			}
		}
		return &voltha.Devices{Items: childDevices}, nil
	}
	return nil, status.Errorf(codes.NotFound, "%s", parentDeviceId)
}

func (dMgr *DeviceManager) setupUNILogicalPorts(cDevice *voltha.Device) error {
	log.Info("addUNILogicalPort")
	if err := dMgr.logicalDeviceMgr.setupUNILogicalPorts(nil, cDevice); err != nil {
		log.Warnw("addUNILogicalPort-error", log.Fields{"device": cDevice, "err": err})
		return err
	}
	return nil
}

func (dMgr *DeviceManager) downloadImage(ctx context.Context, img *voltha.ImageDownload, ch chan interface{}) {
	log.Debugw("downloadImage", log.Fields{"deviceid": img.Id, "imageName": img.Name})
	var res interface{}
	var err error
	if agent := dMgr.getDeviceAgent(img.Id); agent != nil {
		if res, err = agent.downloadImage(ctx, img); err != nil {
			log.Debugw("downloadImage-failed", log.Fields{"err": err, "imageName": img.Name})
			res = err
		}
	} else {
		res = status.Errorf(codes.NotFound, "%s", img.Id)
	}
	sendResponse(ctx, ch, res)
}

func (dMgr *DeviceManager) cancelImageDownload(ctx context.Context, img *voltha.ImageDownload, ch chan interface{}) {
	log.Debugw("cancelImageDownload", log.Fields{"deviceid": img.Id, "imageName": img.Name})
	var res interface{}
	var err error
	if agent := dMgr.getDeviceAgent(img.Id); agent != nil {
		if res, err = agent.cancelImageDownload(ctx, img); err != nil {
			log.Debugw("cancelImageDownload-failed", log.Fields{"err": err, "imageName": img.Name})
			res = err
		}
	} else {
		res = status.Errorf(codes.NotFound, "%s", img.Id)
	}
	sendResponse(ctx, ch, res)
}

func (dMgr *DeviceManager) activateImage(ctx context.Context, img *voltha.ImageDownload, ch chan interface{}) {
	log.Debugw("activateImage", log.Fields{"deviceid": img.Id, "imageName": img.Name})
	var res interface{}
	var err error
	if agent := dMgr.getDeviceAgent(img.Id); agent != nil {
		if res, err = agent.activateImage(ctx, img); err != nil {
			log.Debugw("activateImage-failed", log.Fields{"err": err, "imageName": img.Name})
			res = err
		}
	} else {
		res = status.Errorf(codes.NotFound, "%s", img.Id)
	}
	sendResponse(ctx, ch, res)
}

func (dMgr *DeviceManager) revertImage(ctx context.Context, img *voltha.ImageDownload, ch chan interface{}) {
	log.Debugw("revertImage", log.Fields{"deviceid": img.Id, "imageName": img.Name})
	var res interface{}
	var err error
	if agent := dMgr.getDeviceAgent(img.Id); agent != nil {
		if res, err = agent.revertImage(ctx, img); err != nil {
			log.Debugw("revertImage-failed", log.Fields{"err": err, "imageName": img.Name})
			res = err
		}
	} else {
		res = status.Errorf(codes.NotFound, "%s", img.Id)
	}
	sendResponse(ctx, ch, res)
}

func (dMgr *DeviceManager) getImageDownloadStatus(ctx context.Context, img *voltha.ImageDownload, ch chan interface{}) {
	log.Debugw("getImageDownloadStatus", log.Fields{"deviceid": img.Id, "imageName": img.Name})
	var res interface{}
	var err error
	if agent := dMgr.getDeviceAgent(img.Id); agent != nil {
		if res, err = agent.getImageDownloadStatus(ctx, img); err != nil {
			log.Debugw("getImageDownloadStatus-failed", log.Fields{"err": err, "imageName": img.Name})
			res = err
		}
	} else {
		res = status.Errorf(codes.NotFound, "%s", img.Id)
	}
	sendResponse(ctx, ch, res)
}

func (dMgr *DeviceManager) updateImageDownload(deviceId string, img *voltha.ImageDownload) error {
	log.Debugw("updateImageDownload", log.Fields{"deviceid": img.Id, "imageName": img.Name})
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		if err := agent.updateImageDownload(img); err != nil {
			log.Debugw("updateImageDownload-failed", log.Fields{"err": err, "imageName": img.Name})
			return err
		}
	} else {
		return status.Errorf(codes.NotFound, "%s", img.Id)
	}
	return nil
}

func (dMgr *DeviceManager) getImageDownload(ctx context.Context, img *voltha.ImageDownload) (*voltha.ImageDownload, error) {
	log.Debugw("getImageDownload", log.Fields{"deviceid": img.Id, "imageName": img.Name})
	if agent := dMgr.getDeviceAgent(img.Id); agent != nil {
		return agent.getImageDownload(ctx, img)
	}
	return nil, status.Errorf(codes.NotFound, "%s", img.Id)
}

func (dMgr *DeviceManager) listImageDownloads(ctx context.Context, deviceId string) (*voltha.ImageDownloads, error) {
	log.Debugw("listImageDownloads", log.Fields{"deviceId": deviceId})
	if agent := dMgr.getDeviceAgent(deviceId); agent != nil {
		return agent.listImageDownloads(ctx, deviceId)
	}
	return nil, status.Errorf(codes.NotFound, "%s", deviceId)
}

func (dMgr *DeviceManager) activateDevice(cDevice *voltha.Device) error {
	log.Info("activateDevice")
	return nil
}

func (dMgr *DeviceManager) disableDeviceHandler(cDevice *voltha.Device) error {
	log.Info("disableDevice-donothing")
	return nil
}

func (dMgr *DeviceManager) abandonDevice(cDevice *voltha.Device) error {
	log.Info("abandonDevice")
	return nil
}

func (dMgr *DeviceManager) reEnableDevice(cDevice *voltha.Device) error {
	log.Info("reEnableDevice")
	return nil
}

func (dMgr *DeviceManager) noOp(cDevice *voltha.Device) error {
	log.Info("noOp")
	return nil
}

func (dMgr *DeviceManager) notAllowed(pcDevice *voltha.Device) error {
	log.Info("notAllowed")
	return errors.New("Transition-not-allowed")
}

func funcName(f interface{}) string {
	p := reflect.ValueOf(f).Pointer()
	rf := runtime.FuncForPC(p)
	return rf.Name()
}

func (dMgr *DeviceManager) UpdateDeviceAttribute(deviceId string, attribute string, value interface{}) {
	if agent, ok := dMgr.deviceAgents[deviceId]; ok {
		agent.updateDeviceAttribute(attribute, value)
	}
}

func (dMgr *DeviceManager) GetParentDeviceId(deviceId string) *string {
	if device, _ := dMgr.GetDevice(deviceId); device != nil {
		log.Infow("GetParentDeviceId", log.Fields{"deviceId": device.Id, "parentId": device.ParentId})
		return &device.ParentId
	}
	return nil
}
