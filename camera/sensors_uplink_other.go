//go:build !linux

package main

// UplinkSample mirrors the linux build's struct so callers compile
// on the host dev machine. /sys/class/net is linux-only.
type UplinkSample struct {
	Iface   string
	RxBytes uint64
	TxBytes uint64
}

func ReadUplink() UplinkSample { return UplinkSample{} }
