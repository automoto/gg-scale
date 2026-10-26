package relaynode

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// errBlockedDestination marks a probe target the destination policy refuses.
var errBlockedDestination = errors.New("relaynode: probe destination not allowed")

// metadataAddrs are cloud instance-metadata endpoints outside the link-local
// range, which the policy blocks as a whole.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IPv6
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud
	netip.MustParseAddr("192.0.0.192"),     // Oracle Cloud
}

// checkProbeTarget is the probe's destination policy. Relay nodes serve
// /lifecycle on loopback or a private network, so those stay allowed.
// Link-local (which holds the common 169.254.169.254 metadata address),
// metadata, multicast and unspecified addresses never host a relay and are
// refused, so a registered URL cannot point the API server at them.
func checkProbeTarget(addr netip.Addr) error {
	addr = addr.Unmap()
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() {
		return fmt.Errorf("%w: %s", errBlockedDestination, addr)
	}
	for _, m := range metadataAddrs {
		if addr == m {
			return fmt.Errorf("%w: %s is a cloud metadata address", errBlockedDestination, addr)
		}
	}
	return nil
}

// checkLiteralHost applies the policy to a URL host that is an IP literal, so
// registration refuses it early. A hostname is checked when it is dialed.
func checkLiteralHost(host string) error {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return checkProbeTarget(addr)
}

// controlDial runs on every connection the prober opens, after DNS
// resolution, so a hostname that resolves (or later re-resolves) to a
// blocked address is refused too.
func controlDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: unresolved address %q", errBlockedDestination, address)
	}
	return checkProbeTarget(addr)
}
