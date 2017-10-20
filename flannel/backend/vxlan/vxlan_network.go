// Copyright 2015 flannel authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vxlan

import (
	"encoding/json"
	"net"
	"sync"

	log "github.com/golang/glog"
	"github.com/vishvananda/netlink"
	"golang.org/x/net/context"

	"syscall"

	"github.com/coreos/flannel/backend"
	"github.com/coreos/flannel/pkg/ip"
	"github.com/coreos/flannel/subnet"
)

type network struct {
	backend.SimpleNetwork
	extIface  *backend.ExternalInterface
	dev       *vxlanDevice
	subnetMgr subnet.Manager
}

func newNetwork(subnetMgr subnet.Manager, extIface *backend.ExternalInterface, dev *vxlanDevice, _ ip.IP4Net, lease *subnet.Lease) (*network, error) {
	nw := &network{
		SimpleNetwork: backend.SimpleNetwork{
			SubnetLease: lease,
			ExtIface:    extIface,
		},
		subnetMgr: subnetMgr,
		dev:       dev,
	}

	return nw, nil
}

func (nw *network) Run(ctx context.Context) {
	wg := sync.WaitGroup{}

	log.V(0).Info("watching for new subnet leases")
	events := make(chan []subnet.Event)
	wg.Add(1)
	go func() {
		subnet.WatchLeases(ctx, nw.subnetMgr, nw.SubnetLease, events)
		log.V(1).Info("WatchLeases exited")
		wg.Done()
	}()

	defer wg.Wait()

	for {
		select {
		case evtBatch := <-events:
			nw.handleSubnetEvents(evtBatch)

		case <-ctx.Done():
			return
		}
	}
}

func (nw *network) MTU() int {
	return nw.dev.MTU()
}

type vxlanLeaseAttrs struct {
	VtepMAC hardwareAddr
}

func (nw *network) handleSubnetEvents(batch []subnet.Event) {
	for _, event := range batch {
		sn := event.Lease.Subnet
		attrs := event.Lease.Attrs
		if attrs.BackendType != "vxlan" {
			log.Warningf("ignoring non-vxlan subnet(%s): type=%v", sn, attrs.BackendType)
			continue
		}

		var vxlanAttrs vxlanLeaseAttrs
		if err := json.Unmarshal(attrs.BackendData, &vxlanAttrs); err != nil {
			log.Error("error decoding subnet lease JSON: ", err)
			continue
		}

		// This route is used when traffic should be vxlan encapsulated
		// 创建通往目的subnet的路由
		vxlanRoute := netlink.Route{
			LinkIndex: nw.dev.link.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Dst:       sn.ToIPNet(),
			// 网关为目的主机vtep的地址
			Gw:        sn.IP.ToIP(),
		}
		vxlanRoute.SetFlag(syscall.RTNH_F_ONLINK)

		// directRouting is where the remote host is on the same subnet so vxlan isn't required.
		// 如果远端主机在同一个子网内，则vxlan是没有必要的
		// 相当于直接使用hostgw
		directRoute := netlink.Route{
			Dst: sn.ToIPNet(),
			Gw:  attrs.PublicIP.ToIP(),
		}
		var directRoutingOK = false
		if nw.dev.directRouting {
			routes, err := netlink.RouteGet(attrs.PublicIP.ToIP())
			if err != nil {
				log.Errorf("Couldn't lookup route to %v: %v", attrs.PublicIP, err)
				continue
			}
			if len(routes) == 1 && routes[0].Gw == nil {
				// There is only a single route and there's no gateway (i.e. it's directly connected)
				// 根据目的IP获取的路由没有网关，则说明是直连的
				directRoutingOK = true
			}
		}

		switch event.Type {
		case subnet.EventAdded:
			if directRoutingOK {
				log.V(2).Infof("Adding direct route to subnet: %s PublicIP: %s", sn, attrs.PublicIP)

				// 主机都在同一子网内，则使用directRoute
				if err := netlink.RouteReplace(&directRoute); err != nil {
					log.Errorf("Error adding route to %v via %v: %v", sn, attrs.PublicIP, err)
					continue
				}
			} else {
				log.V(2).Infof("adding subnet: %s PublicIP: %s VtepMAC: %s", sn, attrs.PublicIP, net.HardwareAddr(vxlanAttrs.VtepMAC))
				// 增加到对端VTEP设备的ARP缓存
				if err := nw.dev.AddARP(neighbor{IP: sn.IP, MAC: net.HardwareAddr(vxlanAttrs.VtepMAC)}); err != nil {
					log.Error("AddARP failed: ", err)
					continue
				}

				// 增加fdb，ip为目的主机的Public IP，地址为目的主机vtep设备的mac地址
				if err := nw.dev.AddFDB(neighbor{IP: attrs.PublicIP, MAC: net.HardwareAddr(vxlanAttrs.VtepMAC)}); err != nil {
					log.Error("AddFDB failed: ", err)

					// Try to clean up the ARP entry then continue
					if err := nw.dev.DelARP(neighbor{IP: event.Lease.Subnet.IP, MAC: net.HardwareAddr(vxlanAttrs.VtepMAC)}); err != nil {
						log.Error("DelARP failed: ", err)
					}

					continue
				}

				// Set the route - the kernel would ARP for the Gw IP address if it hadn't already been set above so make sure
				// this is done last.
				if err := netlink.RouteReplace(&vxlanRoute); err != nil {
					log.Errorf("failed to add vxlanRoute (%s -> %s): %v", vxlanRoute.Dst, vxlanRoute.Gw, err)

					// Try to clean up both the ARP and FDB entries then continue
					if err := nw.dev.DelARP(neighbor{IP: event.Lease.Subnet.IP, MAC: net.HardwareAddr(vxlanAttrs.VtepMAC)}); err != nil {
						log.Error("DelARP failed: ", err)
					}

					if err := nw.dev.DelFDB(neighbor{IP: event.Lease.Attrs.PublicIP, MAC: net.HardwareAddr(vxlanAttrs.VtepMAC)}); err != nil {
						log.Error("DelFDB failed: ", err)
					}

					continue
				}
			}
		case subnet.EventRemoved:
			if directRoutingOK {
				log.V(2).Infof("Removing direct route to subnet: %s PublicIP: %s", sn, attrs.PublicIP)
				if err := netlink.RouteDel(&directRoute); err != nil {
					log.Errorf("Error deleting route to %v via %v: %v", sn, attrs.PublicIP, err)
				}
			} else {
				log.V(2).Infof("removing subnet: %s PublicIP: %s VtepMAC: %s", sn, attrs.PublicIP, net.HardwareAddr(vxlanAttrs.VtepMAC))

				// Try to remove all entries - don't bail out if one of them fails.
				if err := nw.dev.DelARP(neighbor{IP: sn.IP, MAC: net.HardwareAddr(vxlanAttrs.VtepMAC)}); err != nil {
					log.Error("DelARP failed: ", err)
				}

				if err := nw.dev.DelFDB(neighbor{IP: attrs.PublicIP, MAC: net.HardwareAddr(vxlanAttrs.VtepMAC)}); err != nil {
					log.Error("DelFDB failed: ", err)
				}

				if err := netlink.RouteDel(&vxlanRoute); err != nil {
					log.Errorf("failed to delete vxlanRoute (%s -> %s): %v", vxlanRoute.Dst, vxlanRoute.Gw, err)
				}
			}
		default:
			log.Error("internal error: unknown event type: ", int(event.Type))
		}
	}
}
