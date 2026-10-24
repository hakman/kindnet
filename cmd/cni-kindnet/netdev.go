/*
Copyright YEAR The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"

	"golang.org/x/sys/unix"
)

const (
	// loopbackIfIndex is the ifindex of lo in every network namespace
	// (LOOPBACK_IFINDEX in the kernel).
	loopbackIfIndex = 1
	// ifAliasMaxLen is the longest alias the kernel accepts (IFALIASZ - 1).
	ifAliasMaxLen = 255
)

func createPodInterface(netconf *NetworkConfig) (err error) {
	iifName := defaultInterface      // veth interface name in the container
	oofName := netconf.InterfaceName // veth interface name in the host

	// Get network namespace file descriptors
	rootNs, err := netns.Get()
	if err != nil {
		return err
	}
	defer rootNs.Close()

	containerNs, err := netns.GetFromPath(netconf.NetNS)
	if err != nil {
		return fmt.Errorf("could not get network namespace from path %s : %w", netconf.NetNS, err)
	}

	// Create veth interface, internal name is eth0 and external is randomly generated
	// with the knet prefix.
	flags := unix.NLM_F_CREATE | unix.NLM_F_EXCL | unix.NLM_F_ACK
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, flags)
	// Get a netlink socket in the container namespace namespace
	s, err := nl.GetNetlinkSocketAt(containerNs, rootNs, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("could not get network namespace handle: %w", err)
	}
	req.Sockets = map[int]*nl.SocketHandle{
		unix.NETLINK_ROUTE: {Socket: s},
	}

	// eth0 is set up as part of the creation: the kernel applies the flags
	// after both ends are linked, so the open does not fail.
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Flags = unix.IFF_UP
	msg.Change = unix.IFF_UP
	req.AddData(msg)

	// internal interface name is always eth0
	nameData := nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(iifName))
	req.AddData(nameData)

	// set MTU if defined, the kernel applies IFLA_MTU when it creates each end
	if netconf.MTU > 0 {
		req.AddData(nl.NewRtAttr(unix.IFLA_MTU, nl.Uint32Attr(uint32(netconf.MTU))))
	}

	// base namespace is the container namespace
	val := nl.Uint32Attr(uint32(containerNs))
	attr := nl.NewRtAttr(unix.IFLA_NET_NS_FD, val)
	req.AddData(attr)

	// the link type is veth
	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated("veth"))

	// veth interface on the root namespace. It can not be set up here:
	// the kernel applies the peer flags before the peer is linked to eth0,
	// and opening an unlinked veth fails with ENOTCONN.
	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	peer := data.AddRtAttr(nl.VETH_INFO_PEER, nil)
	nl.NewIfInfomsgChild(peer, unix.AF_UNSPEC)
	peer.AddRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(oofName))
	if netconf.MTU > 0 {
		peer.AddRtAttr(unix.IFLA_MTU, nl.Uint32Attr(uint32(netconf.MTU)))
	}

	valRoot := nl.Uint32Attr(uint32(rootNs))
	peer.AddRtAttr(unix.IFLA_NET_NS_FD, valRoot)

	req.AddData(linkInfo)

	// execute the netlink operation to create the veth
	_, err = req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return fmt.Errorf("fail to add interface on namespace %s : %v", netconf.NetNS, err)
	}
	// on error, delete the veth pair created above, so a retry in cmdAdd can create it again with the same names
	defer func() {
		if err != nil {
			if delErr := deletePodInterface(iifName, netconf.NetNS); delErr != nil {
				logger.Printf("failed to delete interface %s on namespace %s after a failed setup: %v", iifName, netconf.NetNS, delErr)
			}
		}
	}()

	// veth interface has been created, add routes and addresses now

	// open a netlink handle on the container namespace to use it directly
	// and avoid running goroutines inside network namespaces that does not
	// work fine and require to lock the entire thread.
	// Only NETLINK_ROUTE is used, a handle without a family list opens one
	// socket per supported family.
	// Keep it that way: the library sends a request of another family
	// through a new socket in the current network namespace, not in this one.
	nhNs, err := netlink.NewHandleAt(containerNs, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	// best effort to set the loopback interface up, by index without a lookup
	_ = nhNs.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: loopbackIfIndex}})

	// DAD has to be disabled to avoid latency on network start
	if netconf.IPv6 != nil {
		// set up the veth inside the host outside of the container
		// don't accept Router Advertisements
		_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", oofName), []byte(strconv.Itoa(0)), 0640)
		// DAD increases pod latency and we guarantee IP uniqueness
		_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_dad", oofName), []byte(strconv.Itoa(0)), 0640)
		_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/dad_transmits", oofName), []byte(strconv.Itoa(0)), 0640)

		// set sysctls inside the container
		err = func() error {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			err = netns.Set(containerNs)
			if err != nil {
				return err
			}
			_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", iifName), []byte(strconv.Itoa(0)), 0640)
			// DAD increases pod latency and we guarantee IP uniqueness
			_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_dad", iifName), []byte(strconv.Itoa(0)), 0640)
			_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/dad_transmits", iifName), []byte(strconv.Itoa(0)), 0640)

			netns.Set(rootNs)
			return nil
		}()
		if err != nil {
			return fmt.Errorf("error setting IPv6 sysctl inside the container")
		}
	}

	nhRoot, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("could not get netlink handle on host : %w", err)
	}

	// Set the alias and bring the host side up with one message. The device
	// is selected by name, so it needs no lookup.
	hostReq := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)
	hostMsg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	hostMsg.Flags = unix.IFF_UP
	hostMsg.Change = unix.IFF_UP
	hostReq.AddData(hostMsg)
	hostReq.AddData(nl.NewRtAttr(unix.IFLA_IFNAME, nl.ZeroTerminated(oofName)))
	// set metadata on the alias field with the pod name and namespace
	if netconf.Name != "" {
		alias := fmt.Sprintf("link-pod %s/%s", netconf.Namespace, netconf.Name)
		// the kernel rejects the whole message if the alias is too long,
		// so leave it out, as a failed alias did not fail the ADD before
		if len(alias) <= ifAliasMaxLen {
			hostReq.AddData(nl.NewRtAttr(unix.IFLA_IFALIAS, []byte(alias)))
		} else {
			logger.Printf("could not set interface %s alias : longer than %d bytes", oofName, ifAliasMaxLen)
		}
	}
	if _, err = hostReq.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("failed to set interface %s up: %v", oofName, err)
	}

	// Look up eth0 after both ends are up. The reply carries the host-side
	// ifindex (IFLA_LINK) and the operstate. Since Linux 6.8 the kernel
	// applies the pending link event of the queried device before it
	// answers, so the operstate is UP; older kernels can still report it
	// as down, and then the wait below polls.
	nsLink, err := nhNs.LinkByName(iifName)
	if err != nil && !errors.Is(err, unix.EINTR) {
		return fmt.Errorf("could not get interface %s on namespace %s : %w", iifName, netconf.NetNS, err)
	}

	var hostLink netlink.Link
	if index := nsLink.Attrs().ParentIndex; index > 0 {
		hostLink = &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Index: index, Name: oofName}}
	} else {
		hostLink, err = nhRoot.LinkByName(oofName)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("could not get interface %s on host : %w", oofName, err)
		}
	}

	// Wait until the container-side veth is operationally up before adding routes.
	// A veth reaches OperStateUp only once both ends are up, at which point the
	// kernel's IPv6 stack is fully initialized and route additions will not race.
	if nsLink.Attrs().OperState != netlink.OperUp {
		if err = waitForLinkOperational(nhNs, nsLink.Attrs().Index); err != nil {
			return fmt.Errorf("interface %s did not become operational: %w", iifName, err)
		}
	}

	// Configure addresses and routes
	if netconf.IPv4 != nil {
		// add the Pod address as a /32 inside the namespace and use the "onlink" flag on the
		// default gateway to avoid arp. The default gateway is added to the lo interface on the
		// veth host.
		address := &netlink.Addr{IPNet: &net.IPNet{IP: netconf.IPv4, Mask: net.CIDRMask(32, 32)}}
		err = nhNs.AddrAdd(nsLink, address)
		if err != nil {
			return fmt.Errorf("could not add address %s on namespace %s : %w", netconf.IPv4.String(), netconf.NetNS, err)
		}

		addressGw := &netlink.Addr{IPNet: &net.IPNet{IP: netconf.GWv4, Mask: net.CIDRMask(32, 32)}}
		err = nhRoot.AddrAdd(hostLink, addressGw)
		if err != nil {
			return fmt.Errorf("could not add address %s on interface %s : %w", netconf.IPv4.String(), oofName, err)
		}

		// set the route from the network namespace to the host
		routeToGateway := netlink.Route{
			LinkIndex: nsLink.Attrs().Index,
			Scope:     netlink.SCOPE_LINK,
			Dst:       addressGw.IPNet,
		}

		if err := nhNs.RouteAdd(&routeToGateway); err != nil {
			return fmt.Errorf("could not add route to default gw on namespace %s : %w", netconf.NetNS, err)
		}

		// set the route from the network namespace to the host
		defaultRoute := netlink.Route{
			LinkIndex: nsLink.Attrs().Index,
			Src:       netconf.IPv4,
			Gw:        netconf.GWv4,
			Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 0)},
		}
		if err := nhNs.RouteAdd(&defaultRoute); err != nil {
			return fmt.Errorf("could not add default route on namespace %s : %w", netconf.NetNS, err)
		}

		// set the route from the host to the network namespace
		route := netlink.Route{
			LinkIndex: hostLink.Attrs().Index,
			Src:       netconf.GWv4,
			Scope:     netlink.SCOPE_HOST,
			Dst:       address.IPNet,
		}
		if err := nhRoot.RouteAdd(&route); err != nil {
			return fmt.Errorf("could not add route %v on host to the container interface %s : %w", route, hostLink.Attrs().Name, err)
		}
	}

	if netconf.IPv6 != nil {
		// add the Pod address as a /128 inside the namespace and use the "onlink" flag on the
		// default gateway to avoid discovery. The default gateway is added to the veth host interface.
		address := &netlink.Addr{
			IPNet: &net.IPNet{IP: netconf.IPv6, Mask: net.CIDRMask(128, 128)},
			Flags: unix.IFA_F_PERMANENT,
		}
		err = nhNs.AddrAdd(nsLink, address)
		if err != nil {
			return fmt.Errorf("could not add address %s on namespace %s : %w", netconf.IPv6.String(), netconf.NetNS, err)
		}

		addressGw := &netlink.Addr{
			IPNet: &net.IPNet{IP: netconf.GWv6, Mask: net.CIDRMask(128, 128)},
			Flags: unix.IFA_F_PERMANENT,
		}
		err = nhRoot.AddrAdd(hostLink, addressGw)
		if err != nil {
			return fmt.Errorf("could not add address %s on interface %s : %w", netconf.GWv6.String(), oofName, err)
		}
		// The host-side route uses Src: GWv6; the kernel validates that GWv6
		// has a local route (table 255) before accepting it. Wait until that
		// local route is visible before proceeding.
		if err = waitForLocalAddr(nhRoot, netconf.GWv6); err != nil {
			return fmt.Errorf("gateway address %s not ready on host: %w", netconf.GWv6.String(), err)
		}

		// set the route from the network namespace to the host
		routeToGateway := netlink.Route{
			LinkIndex: nsLink.Attrs().Index,
			Scope:     netlink.SCOPE_LINK,
			Dst:       addressGw.IPNet,
		}

		if err := nhNs.RouteAdd(&routeToGateway); err != nil {
			return fmt.Errorf("could not add route to default gw on namespace %s : %w", netconf.NetNS, err)
		}

		// set the route from the network namespace to the host
		defaultRoute := netlink.Route{
			LinkIndex: nsLink.Attrs().Index,
			Gw:        netconf.GWv6,
			Dst:       &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 0)},
		}

		if err := nhNs.RouteAdd(&defaultRoute); err != nil {
			return fmt.Errorf("could not add default route on namespace %s : %w", netconf.NetNS, err)
		}

		route := netlink.Route{
			LinkIndex: hostLink.Attrs().Index,
			Src:       netconf.GWv6,
			Scope:     netlink.SCOPE_HOST,
			Dst:       address.IPNet,
		}
		if err := nhRoot.RouteAdd(&route); err != nil {
			return fmt.Errorf("could not add route %v on the host to the container interface %s : %w", route, hostLink.Attrs().Name, err)
		}
	}
	return nil
}

// waitForLocalAddr polls RouteGet using nh until the kernel has added a local
// route for addr (type RTN_LOCAL), confirming the address is fully committed
// to the kernel's routing tables and usable as a route preferred source.
// RouteGet is a targeted lookup (not a dump) so it is reliable under concurrent
// netlink load. Times out after one second.
func waitForLocalAddr(nh *netlink.Handle, addr net.IP) error {
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		routes, err := nh.RouteGet(addr)
		if err == nil {
			for _, r := range routes {
				if r.Type == unix.RTN_LOCAL {
					return nil
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("local route for %s not ready after 1s", addr)
}

// waitForLinkOperational polls the link at linkIdx using nh until it reaches
// OperStateUp, indicating the kernel's networking stack (including IPv6) is
// fully initialized and ready for route additions. For veth pairs this state
// is reached only once both ends are up, so callers should bring both ends up
// before calling this. Times out after one second.
func waitForLinkOperational(nh *netlink.Handle, linkIdx int) error {
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		link, err := nh.LinkByIndex(linkIdx)
		if err != nil {
			return err
		}
		if link.Attrs().OperState == netlink.LinkOperState(netlink.OperUp) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for link index %d to become operational", linkIdx)
}

func deletePodInterface(ifName string, netNS string) error {
	containerNs, err := netns.GetFromPath(netNS)
	if err != nil {
		return fmt.Errorf("could not get network namespace from path %s for network device %s : %w", netNS, ifName, err)
	}
	// to avoid golang problem with goroutines we create the socket in the
	// namespace and use it directly, only for NETLINK_ROUTE
	nhNs, err := netlink.NewHandleAt(containerNs, unix.NETLINK_ROUTE)
	if err != nil {
		// namespace no longer exist
		return nil
	}

	nsLink, err := nhNs.LinkByName(ifName)
	if err != nil && !errors.Is(err, unix.EINTR) {
		// interface is no present so no need to delete
		return nil
	}

	err = nhNs.LinkDel(nsLink)
	if err != nil {
		return fmt.Errorf("failed to delete %q: %v", ifName, err)
	}
	return nil
}

func getDefaultGwInterfaceMTU() int {
	filter := &netlink.Route{
		Table: unix.RT_TABLE_MAIN,
	}
	// Dump one family at a time. The dump of all families always runs under
	// RTNL, while since Linux 6.9 (IPv4) and 6.10 (IPv6) the dump of one
	// family does not. IPv4 is preferred, so the IPv6 routes are only read
	// without an IPv4 default route.
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		routes, err := netlink.RouteListFiltered(family, filter, netlink.RT_FILTER_TABLE)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return 0
		}
		bestLink, ok := defaultRouteLink(routes)
		if !ok {
			continue
		}
		intfLink, err := netlink.LinkByIndex(bestLink)
		if err != nil {
			return 0
		}
		return intfLink.Attrs().MTU
	}
	return 0
}

// defaultRouteLink returns the link of the default route with the lowest
// metric, routes must belong to a single family.
func defaultRouteLink(routes []netlink.Route) (int, bool) {
	minMetric := math.MaxInt32
	var bestLink int
	found := false

	for _, r := range routes {
		if r.Dst != nil {
			ones, bits := r.Dst.Mask.Size()
			if !r.Dst.IP.IsUnspecified() || ones != 0 || (bits != 32 && bits != 128) {
				continue
			}
		}

		metric := r.Priority

		var linkIndex int
		if len(r.MultiPath) > 0 {
			linkIndex = r.MultiPath[0].LinkIndex
		} else {
			linkIndex = r.LinkIndex
		}

		if metric < minMetric {
			minMetric = metric
			bestLink = linkIndex
			found = true
		}
	}
	return bestLink, found
}
