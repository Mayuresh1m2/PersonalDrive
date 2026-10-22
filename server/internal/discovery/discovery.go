// Package discovery advertises the server on the LAN via mDNS / DNS-SD so
// apps can find it without anyone typing an IP address.
package discovery

import (
	"github.com/grandcat/zeroconf"
)

const ServiceType = "_pdrive._tcp"

// Advertise registers the service. Call Shutdown on the result to stop.
// caFingerprint lets clients verify they found the right server.
func Advertise(name string, port int, caFingerprint, version string) (*zeroconf.Server, error) {
	txt := []string{"v=1", "version=" + version, "ca=" + caFingerprint}
	return zeroconf.Register(name, ServiceType, "local.", port, txt, nil)
}
