//go:build !linux

package main

// gpsdQuery is a no-op on non-Linux platforms.
func gpsdQuery() (*float64, *float64, *float32, *uint8) {
	return nil, nil, nil, nil
}
