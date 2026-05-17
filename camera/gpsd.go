//go:build linux

package main

// Persistent gpsd WATCH client + cached fix.
//
// Earlier shape: gpsdQuery opened a fresh TCP connection, sent ?WATCH,
// and read lines until the first valid TPV — synchronous, called once
// per telemetry tick. On the SIM7600 the first TPV often took the full
// 5s timeout (gpsd's NMEA stream is paced by the receiver's 1 Hz output
// and isn't aligned with the WATCH command), stalling the telemetry
// loop. Pi field test 2026-05-13 showed gpsd_query_ms ≈ 5000 every
// cycle — see Python commit 12af133 for the same fix in the Python
// camera, ported here.
//
// New shape: StartGpsdReader spawns a single goroutine that holds a
// ?WATCH stream open forever, decoding TPV objects into a shared
// cache. gpsdQuery snapshots the cache under a lock and returns in
// <1 ms. Fixes older than gpsdStaleAfter are returned as nil so a
// disconnected receiver doesn't pin the camera at its last-known
// location.

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"time"
)

const (
	gpsdAddr        = "localhost:2947"
	gpsdDialTimeout = 5 * time.Second
	gpsdStaleAfter  = 30 * time.Second
	gpsdRetryDelay  = 5 * time.Second
)

// tpvReport is the gpsd TPV (Time-Position-Velocity) JSON object.
type tpvReport struct {
	Class  string  `json:"class"`
	Mode   int     `json:"mode"` // 0=unknown, 1=no fix, 2=2D, 3=3D
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	AltHAE float64 `json:"altHAE"` // altitude above ellipsoid (metres)
	Alt    float64 `json:"alt"`    // altitude MSL (metres), older gpsd
}

type gpsdFix struct {
	lat, lon float64
	alt      float32
	mode     uint8 // 0=unknown, 2=2D, 3=3D
	updated  time.Time
}

var (
	gpsdMu    sync.RWMutex
	gpsdLast  gpsdFix
	gpsdReady bool
)

// gpsdQuery returns the most recently cached fix, or nils if no fix is
// available or the last update is older than gpsdStaleAfter. Sub-
// millisecond — the heavy lifting happens in the persistent reader
// goroutine launched by StartGpsdReader.
func gpsdQuery() (*float64, *float64, *float32, *uint8) {
	gpsdMu.RLock()
	defer gpsdMu.RUnlock()
	if !gpsdReady {
		return nil, nil, nil, nil
	}
	if time.Since(gpsdLast.updated) > gpsdStaleAfter {
		return nil, nil, nil, nil
	}
	lat := gpsdLast.lat
	lon := gpsdLast.lon
	alt := gpsdLast.alt
	mode := gpsdLast.mode
	return &lat, &lon, &alt, &mode
}

// StartGpsdReader runs the persistent gpsd reader until ctx is cancelled.
// On connection failure it retries every gpsdRetryDelay. Safe to call
// from main as a background goroutine; idempotent if already running
// (subsequent calls just spawn another reader which will fight for the
// cache — don't do that).
func StartGpsdReader(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := gpsdReadOnce(ctx); err != nil && ctx.Err() == nil {
			// Couldn't connect or stream died — back off and retry.
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(gpsdRetryDelay):
		}
	}
}

func gpsdReadOnce(ctx context.Context) error {
	d := net.Dialer{Timeout: gpsdDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", gpsdAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Close on ctx cancellation.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)

	// Skip the version banner.
	if !scanner.Scan() {
		return scanner.Err()
	}
	if _, err := conn.Write([]byte(`?WATCH={"enable":true,"json":true}` + "\n")); err != nil {
		return err
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		var head struct {
			Class string `json:"class"`
		}
		if json.Unmarshal(line, &head) != nil || head.Class != "TPV" {
			continue
		}
		var tpv tpvReport
		if json.Unmarshal(line, &tpv) != nil {
			continue
		}
		if tpv.Mode < 2 {
			continue
		}
		alt := tpv.AltHAE
		if alt == 0 {
			alt = tpv.Alt
		}
		gpsdMu.Lock()
		gpsdLast = gpsdFix{
			lat:     tpv.Lat,
			lon:     tpv.Lon,
			alt:     float32(alt),
			mode:    uint8(tpv.Mode),
			updated: time.Now(),
		}
		gpsdReady = true
		gpsdMu.Unlock()
	}
	return scanner.Err()
}
