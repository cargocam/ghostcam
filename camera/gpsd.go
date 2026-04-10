//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"time"
)

const (
	gpsdAddr    = "localhost:2947"
	gpsdTimeout = 5 * time.Second
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

// gpsdQuery connects to gpsd, requests a watch, and reads until it gets a TPV
// report with a fix. Returns nils if gpsd is unavailable or has no fix.
func gpsdQuery() (*float64, *float64, *float32, *uint8) {
	conn, err := net.DialTimeout("tcp", gpsdAddr, gpsdTimeout)
	if err != nil {
		return nil, nil, nil, nil
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(gpsdTimeout))

	// Read the initial gpsd version banner
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return nil, nil, nil, nil
	}

	// Enable JSON watch mode
	_, err = conn.Write([]byte(`?WATCH={"enable":true,"json":true}` + "\n"))
	if err != nil {
		return nil, nil, nil, nil
	}

	// Read lines until we find a TPV report or timeout
	for scanner.Scan() {
		line := scanner.Bytes()

		var raw struct {
			Class string `json:"class"`
		}
		if json.Unmarshal(line, &raw) != nil || raw.Class != "TPV" {
			continue
		}

		var tpv tpvReport
		if json.Unmarshal(line, &tpv) != nil {
			continue
		}

		if tpv.Mode < 2 {
			// No fix yet — keep reading for a better report
			continue
		}

		alt := tpv.AltHAE
		if alt == 0 {
			alt = tpv.Alt // fallback for older gpsd
		}
		altF := float32(alt)
		fix := uint8(tpv.Mode) // 2=2D, 3=3D

		return &tpv.Lat, &tpv.Lon, &altF, &fix
	}

	return nil, nil, nil, nil
}
