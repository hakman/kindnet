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
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestDefaultRouteLink(t *testing.T) {
	mustCIDR := func(s string) *net.IPNet {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	tests := []struct {
		name      string
		routes    []netlink.Route
		wantLink  int
		wantFound bool
	}{
		{
			name: "no routes",
		},
		{
			name:   "no default route",
			routes: []netlink.Route{{LinkIndex: 2, Dst: mustCIDR("10.0.0.0/8")}},
		},
		{
			name:      "default route without destination",
			routes:    []netlink.Route{{LinkIndex: 2, Priority: 100}},
			wantLink:  2,
			wantFound: true,
		},
		{
			name:      "IPv4 default route with destination",
			routes:    []netlink.Route{{LinkIndex: 3, Dst: mustCIDR("0.0.0.0/0")}},
			wantLink:  3,
			wantFound: true,
		},
		{
			name:      "IPv6 default route with destination",
			routes:    []netlink.Route{{LinkIndex: 4, Dst: mustCIDR("::/0")}},
			wantLink:  4,
			wantFound: true,
		},
		{
			name: "lowest metric wins",
			routes: []netlink.Route{
				{LinkIndex: 2, Priority: 200},
				{LinkIndex: 3, Dst: mustCIDR("10.0.0.0/8"), Priority: 10},
				{LinkIndex: 4, Priority: 100},
			},
			wantLink:  4,
			wantFound: true,
		},
		{
			name: "first route wins on equal metrics",
			routes: []netlink.Route{
				{LinkIndex: 5, Priority: 100},
				{LinkIndex: 6, Priority: 100},
			},
			wantLink:  5,
			wantFound: true,
		},
		{
			name: "multipath uses the first next hop",
			routes: []netlink.Route{{MultiPath: []*netlink.NexthopInfo{
				{LinkIndex: 7},
				{LinkIndex: 8},
			}}},
			wantLink:  7,
			wantFound: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link, found := defaultRouteLink(tt.routes)
			if link != tt.wantLink || found != tt.wantFound {
				t.Errorf("defaultRouteLink() = (%d, %v), want (%d, %v)", link, found, tt.wantLink, tt.wantFound)
			}
		})
	}
}
