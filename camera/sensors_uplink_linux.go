//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// UplinkSample is the current default-route interface plus its
// monotonic-since-boot byte counters. Iface is "" when no default
// route is present (e.g. modem still attaching at first boot); the
// caller treats that as "no data."
type UplinkSample struct {
	Iface   string
	RxBytes uint64
	TxBytes uint64
}

// ReadUplink samples the current default-route interface and its
// /sys/class/net/<iface>/statistics counters. Synthetic builds use
// this too (build tag linux only, not gated on !synthetic) — even
// without a real capture pipeline the host's network counters are
// real and harmless to read, and the synthetic dummy-cameras flow
// benefits from seeing realistic uplink numbers in dashboards.
//
// On non-linux dev machines this is shadowed by sensors_uplink_other.go.
func ReadUplink() UplinkSample {
	iface := readDefaultInterface()
	if iface == "" {
		return UplinkSample{}
	}
	rx, rxOK := readNetStat(iface, "rx_bytes")
	tx, txOK := readNetStat(iface, "tx_bytes")
	if !rxOK && !txOK {
		// We have an iface name but no counters — possible if the
		// iface was just torn down between readDefaultInterface and
		// the stat reads. Surface the name anyway so the server can
		// see what we thought the uplink was.
		return UplinkSample{Iface: iface}
	}
	return UplinkSample{Iface: iface, RxBytes: rx, TxBytes: tx}
}

func readNetStat(iface, name string) (uint64, bool) {
	path := "/sys/class/net/" + iface + "/statistics/" + name
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
