// Package remoteaddr parses, validates, and classifies the typed remote
// addresses players share for peer-to-peer play: IP addresses, DNS names,
// and Iroh endpoint IDs. LAN vs public scope is derived from the value
// server-side, never chosen by the user.
package remoteaddr

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Type is the user-facing address format.
type Type string

// The wire/form values for each address type.
const (
	TypeIP   Type = "ip"
	TypeDNS  Type = "dns"
	TypeIroh Type = "iroh"
)

// Scope is the derived reachability of an address.
type Scope string

// ScopeNone means no reachability claim (iroh, unclassified DNS).
const (
	ScopeNone   Scope = ""
	ScopeLAN    Scope = "lan"
	ScopePublic Scope = "public"
)

// Slot identifies which storage column an address occupies. At most one
// address per slot; IPs split into LAN/public slots by detected scope.
type Slot int

// The four slots, in display order.
const (
	SlotIPLAN Slot = iota
	SlotIPPublic
	SlotDNS
	SlotIroh
)

// Label returns the user-facing name of the slot, phrased to fit
// "a <label> is already set".
func (s Slot) Label() string {
	switch s {
	case SlotIPLAN:
		return "LAN IP address"
	case SlotIPPublic:
		return "public IP address"
	case SlotDNS:
		return "DNS name"
	default:
		return "Iroh endpoint ID"
	}
}

const (
	maxLen      = 255
	maxHostLen  = 253
	maxLabelLen = 63
	irohKeyLen  = 32
)

// Parse errors are user-safe: handlers pass them straight into HTTP 400
// bodies and form flash messages.
var (
	errNotIP       = errors.New("not a valid IP address")
	errIPNotUsable = errors.New("IP address must be a unicast address without a zone")
	errBadPort     = errors.New("port must be between 1 and 65535")
	errNotHostname = errors.New("not a valid DNS name")
	errDNSIsIP     = errors.New("IP literals must use the IP address type")
	errNotIroh     = errors.New("not a valid Iroh endpoint ID (expected 64 hex characters)")
	errTooLong     = errors.New("address too long")
	errUnknownType = errors.New("unknown address type")
)

// cgnat is the RFC 6598 carrier-grade NAT range, LAN-ish for our purposes:
// peers outside the carrier's network can never dial it.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

var lanDNSSuffixes = []string{".local", ".internal", ".lan", ".home.arpa"}

// Address is a validated, normalized remote address. Value may embed a
// ":port" for ip/dns types.
type Address struct {
	Type  Type
	Scope Scope
	Value string
}

// ParseType maps a wire/form string to a Type.
func ParseType(s string) (Type, bool) {
	switch Type(s) {
	case TypeIP, TypeDNS, TypeIroh:
		return Type(s), true
	}
	return "", false
}

// Parse validates raw as an address of type t, returning the normalized,
// scope-classified Address.
func Parse(t Type, raw string) (Address, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > maxLen {
		return Address{}, errTooLong
	}
	switch t {
	case TypeIP:
		return parseIP(raw)
	case TypeDNS:
		return parseDNS(raw)
	case TypeIroh:
		return parseIroh(raw)
	}
	return Address{}, errUnknownType
}

func parseIP(raw string) (Address, error) {
	// ParseAddrPort requires brackets for IPv6-with-port, so a bare
	// "v6::addr:1234" falls through and parses as a plain address.
	if ap, err := netip.ParseAddrPort(raw); err == nil {
		addr := ap.Addr().Unmap()
		if err := usableIP(addr); err != nil {
			return Address{}, err
		}
		if ap.Port() == 0 {
			return Address{}, errBadPort
		}
		value := netip.AddrPortFrom(addr, ap.Port()).String()
		return Address{Type: TypeIP, Scope: ipScope(addr), Value: value}, nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return Address{}, errNotIP
	}
	addr = addr.Unmap()
	if err := usableIP(addr); err != nil {
		return Address{}, err
	}
	return Address{Type: TypeIP, Scope: ipScope(addr), Value: addr.String()}, nil
}

func usableIP(addr netip.Addr) error {
	if addr.Zone() != "" || addr.IsUnspecified() || addr.IsMulticast() {
		return errIPNotUsable
	}
	return nil
}

func ipScope(addr netip.Addr) Scope {
	if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsPrivate() || cgnat.Contains(addr) {
		return ScopeLAN
	}
	return ScopePublic
}

func parseDNS(raw string) (Address, error) {
	host, port := raw, ""
	if strings.Contains(raw, ":") {
		h, p, err := net.SplitHostPort(raw)
		if err != nil {
			return Address{}, errNotHostname
		}
		if p == "" {
			return Address{}, errBadPort
		}
		host, port = h, p
	}
	host = strings.ToLower(host)
	if _, err := netip.ParseAddr(host); err == nil {
		return Address{}, errDNSIsIP
	}
	if !validHostname(host) {
		return Address{}, errNotHostname
	}
	value := host
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return Address{}, errBadPort
		}
		value = host + ":" + strconv.FormatUint(n, 10)
	}
	return Address{Type: TypeDNS, Scope: dnsScope(host), Value: value}, nil
}

// validHostname checks RFC 1123: dot-separated labels of [a-z0-9-],
// no leading/trailing hyphen, 1-63 chars each, 253 total. host must
// already be lowercased.
func validHostname(host string) bool {
	if host == "" || len(host) > maxHostLen || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !validHostLabel(label) {
			return false
		}
	}
	return true
}

func validHostLabel(label string) bool {
	if label == "" || len(label) > maxLabelLen {
		return false
	}
	if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	for _, r := range label {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		if !lower && !digit && r != '-' {
			return false
		}
	}
	return true
}

func dnsScope(host string) Scope {
	for _, suffix := range lanDNSSuffixes {
		if strings.HasSuffix(host, suffix) {
			return ScopeLAN
		}
	}
	return ScopeNone
}

func parseIroh(raw string) (Address, error) {
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != irohKeyLen {
		return Address{}, errNotIroh
	}
	return Address{Type: TypeIroh, Value: strings.ToLower(raw)}, nil
}

// Slot returns the storage slot this address occupies.
func (a Address) Slot() Slot {
	switch a.Type {
	case TypeIP:
		if a.Scope == ScopeLAN {
			return SlotIPLAN
		}
		return SlotIPPublic
	case TypeDNS:
		return SlotDNS
	default:
		return SlotIroh
	}
}

// FromStored rebuilds an Address from a stored slot value. DNS scope is
// re-derived from the host; IP scope comes from the slot identity.
func FromStored(s Slot, value string) Address {
	switch s {
	case SlotIPLAN:
		return Address{Type: TypeIP, Scope: ScopeLAN, Value: value}
	case SlotIPPublic:
		return Address{Type: TypeIP, Scope: ScopePublic, Value: value}
	case SlotDNS:
		host := value
		if h, _, err := net.SplitHostPort(value); err == nil {
			host = h
		}
		return Address{Type: TypeDNS, Scope: dnsScope(host), Value: value}
	default:
		return Address{Type: TypeIroh, Value: value}
	}
}

// Set holds at most one address per slot: a player's complete shareable
// address book.
type Set struct {
	IPLAN, IPPublic, DNS, Iroh *Address
}

// NewSet assigns each address to its slot, rejecting duplicates.
func NewSet(addrs []Address) (Set, error) {
	var set Set
	for i := range addrs {
		slot := addrs[i].Slot()
		p := set.slotPtr(slot)
		if *p != nil {
			return Set{}, fmt.Errorf("a %s is already set", slot.Label())
		}
		*p = &addrs[i]
	}
	return set, nil
}

// SetFromValues rebuilds a Set from the four stored slot columns.
func SetFromValues(ipLAN, ipPublic, dns, iroh *string) Set {
	var set Set
	fill := func(slot Slot, value *string) {
		if value == nil {
			return
		}
		a := FromStored(slot, *value)
		*set.slotPtr(slot) = &a
	}
	fill(SlotIPLAN, ipLAN)
	fill(SlotIPPublic, ipPublic)
	fill(SlotDNS, dns)
	fill(SlotIroh, iroh)
	return set
}

func (s *Set) slotPtr(slot Slot) **Address {
	switch slot {
	case SlotIPLAN:
		return &s.IPLAN
	case SlotIPPublic:
		return &s.IPPublic
	case SlotDNS:
		return &s.DNS
	default:
		return &s.Iroh
	}
}

// List returns the set's addresses in display order: LAN IP, public IP,
// DNS, Iroh.
func (s Set) List() []Address {
	var out []Address
	for _, p := range []*Address{s.IPLAN, s.IPPublic, s.DNS, s.Iroh} {
		if p != nil {
			out = append(out, *p)
		}
	}
	return out
}
