//go:build linux && !synthetic

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log/slog"
	"os/exec"
	"syscall"
	"time"

	"github.com/cargocam/ghostcam/common"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

const (
	qrWidth    = 640
	qrHeight   = 480
	qrYUVSize  = qrWidth * qrHeight * 3 / 2 // YUV420 frame size
	qrYSize    = qrWidth * qrHeight          // Y plane only (luminance)
	qrTimeout  = 5 * time.Minute
)

// ScanQR captures frames from rpicam-still and scans for a provisioning QR code.
// Returns nil, nil if rpicam-still is unavailable or no QR is found before timeout.
func ScanQR(ctx context.Context) (*common.QRPayload, error) {
	if _, err := exec.LookPath("rpicam-still"); err != nil {
		slog.Debug("rpicam-still not found, skipping QR scan")
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, qrTimeout)
	defer cancel()

	slog.Info("scanning for provisioning QR code", "timeout", qrTimeout)

	cmd := exec.CommandContext(ctx, "rpicam-still",
		"--width", fmt.Sprint(qrWidth),
		"--height", fmt.Sprint(qrHeight),
		"-n",           // no preview
		"-t", "0",      // run indefinitely
		"--timelapse", "500", // capture every 500ms
		"--encoding", "yuv420",
		"-o", "-", // stdout
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pgid := -cmd.Process.Pid
		if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
			return err
		}
		go func() {
			time.Sleep(5 * time.Second)
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}()
		return nil
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting rpicam-still: %w", err)
	}
	slog.Debug("rpicam-still started", "pid", cmd.Process.Pid)

	reader := qrcode.NewQRCodeReader()
	buf := make([]byte, qrYUVSize)

	for {
		if ctx.Err() != nil {
			break
		}

		_, err := io.ReadFull(stdout, buf)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Debug("frame read error", "err", err)
			break
		}

		// Extract Y plane (luminance) from YUV420 — first qrYSize bytes
		gray := &image.Gray{
			Pix:    buf[:qrYSize],
			Stride: qrWidth,
			Rect:   image.Rect(0, 0, qrWidth, qrHeight),
		}

		bitmap, err := gozxing.NewBinaryBitmapFromImage(gray)
		if err != nil {
			continue
		}

		result, err := reader.Decode(bitmap, nil)
		if err != nil {
			continue // no QR found in this frame
		}

		var payload common.QRPayload
		if err := json.Unmarshal([]byte(result.GetText()), &payload); err != nil {
			slog.Debug("QR decoded but not valid JSON", "text", result.GetText())
			continue
		}

		if payload.Server == "" || payload.Token == "" {
			slog.Debug("QR payload missing server or token", "payload", result.GetText())
			continue
		}

		slog.Info("QR code decoded", "server", payload.Server)
		cancel()
		_ = cmd.Wait()
		return &payload, nil
	}

	_ = cmd.Wait()
	slog.Info("QR scan timed out, no QR code found")
	return nil, nil
}
