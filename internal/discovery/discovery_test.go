package discovery

import (
	"net"
	"reflect"
	"testing"

	"github.com/libp2p/zeroconf/v2"

	"picam-frontend/internal/config"
)

func entry(instance string, port int, text []string, ipv4, ipv6 []string) *zeroconf.ServiceEntry {
	e := &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{Instance: instance},
		Port:          port,
		Text:          text,
	}
	for _, ip := range ipv4 {
		e.AddrIPv4 = append(e.AddrIPv4, net.ParseIP(ip))
	}
	for _, ip := range ipv6 {
		e.AddrIPv6 = append(e.AddrIPv6, net.ParseIP(ip))
	}
	return e
}

func TestBackendsFromEntries(t *testing.T) {
	entries := []*zeroconf.ServiceEntry{
		entry("front", 81, []string{"label=Front Yard"}, []string{"10.10.0.50"}, nil),
		entry("back", 81, nil, []string{"10.10.0.51"}, nil), // no label TXT -> falls back to instance name
		entry("dual", 81, []string{"label=Dual Stack"}, []string{"10.10.0.52"}, []string{"fe80::1"}), // IPv4 preferred
		entry("v6only", 81, []string{"label=V6 Only"}, nil, []string{"fe80::2"}),
		entry("noaddr", 81, []string{"label=No Address"}, nil, nil), // skipped: no usable address
	}

	got := backendsFromEntries(entries)

	want := []config.Backend{
		{Name: "front", Label: "Front Yard", Host: "10.10.0.50", Port: 81},
		{Name: "back", Label: "back", Host: "10.10.0.51", Port: 81},
		{Name: "dual", Label: "Dual Stack", Host: "10.10.0.52", Port: 81},
		{Name: "v6only", Label: "V6 Only", Host: "fe80::2", Port: 81},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("backendsFromEntries() = %+v, want %+v", got, want)
	}
}

func TestBackendsFromEntriesEmpty(t *testing.T) {
	if got := backendsFromEntries(nil); got != nil {
		t.Fatalf("backendsFromEntries(nil) = %+v, want nil", got)
	}
}
