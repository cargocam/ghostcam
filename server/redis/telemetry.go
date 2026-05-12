package redis

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/cargocam/ghostcam/common"
	"github.com/cargocam/ghostcam/server/apitypes"
	goredis "github.com/redis/go-redis/v9"
)

const (
	telemetryKeyPrefix = "telemetry:"
	retentionMs        = 24 * 60 * 60 * 1000 // 24 hours
)

// WriteTelemetry writes a telemetry datagram to Redis using XADD with MINID trimming.
func WriteTelemetry(ctx context.Context, rdb *goredis.Client, deviceID string, d *common.TelemetryDatagram) {
	key := telemetryKeyPrefix + deviceID
	serverTS := uint64(time.Now().UnixMilli())
	minID := serverTS - retentionMs

	fields := datagramToFields(d, serverTS)

	if d.Lat != nil {
		slog.Debug("telemetry has GPS", "device_id", deviceID, "lat", *d.Lat, "lon", *d.Lon)
	}

	err := rdb.XAdd(ctx, &goredis.XAddArgs{
		Stream: key,
		MinID:  fmt.Sprintf("%d", minID),
		Approx: true,
		ID:     "*",
		Values: fields,
	}).Err()
	if err != nil {
		slog.Debug("redis telemetry write error", "device_id", deviceID, "error", err)
	}
}

func datagramToFields(d *common.TelemetryDatagram, serverTS uint64) map[string]interface{} {
	fields := map[string]interface{}{
		"ts":        strconv.FormatUint(d.TS, 10),
		"server_ts": strconv.FormatUint(serverTS, 10),
	}
	if d.Sig != nil {
		fields["sig"] = strconv.FormatInt(int64(*d.Sig), 10)
	}
	if d.Temp != nil {
		fields["temp"] = strconv.FormatUint(uint64(*d.Temp), 10)
	}
	if d.FPS != nil {
		fields["fps"] = strconv.FormatFloat(float64(*d.FPS), 'f', -1, 32)
	}
	if d.Kbps != nil {
		fields["kbps"] = strconv.FormatUint(uint64(*d.Kbps), 10)
	}
	if d.CPU != nil {
		fields["cpu"] = strconv.FormatUint(uint64(*d.CPU), 10)
	}
	if d.Mem != nil {
		fields["mem"] = strconv.FormatUint(uint64(*d.Mem), 10)
	}
	if d.Uptime != nil {
		fields["uptime"] = strconv.FormatUint(uint64(*d.Uptime), 10)
	}
	if d.Lat != nil {
		fields["lat"] = strconv.FormatFloat(*d.Lat, 'f', -1, 64)
	}
	if d.Lon != nil {
		fields["lon"] = strconv.FormatFloat(*d.Lon, 'f', -1, 64)
	}
	if d.Alt != nil {
		fields["alt"] = strconv.FormatFloat(float64(*d.Alt), 'f', -1, 32)
	}
	if d.GPSFix != nil {
		fields["gps_fix"] = strconv.FormatUint(uint64(*d.GPSFix), 10)
	}
	// --- power-mode + bandwidth-savings fields (GH #75 / #76 series) ---
	if d.PowerMode != nil {
		fields["power_mode"] = *d.PowerMode
	}
	if d.UploadMode != nil {
		fields["upload_mode"] = *d.UploadMode
	}
	if d.BatteryPct != nil {
		fields["battery_pct"] = strconv.FormatUint(uint64(*d.BatteryPct), 10)
	}
	if d.MotionSegmentsUploaded != nil {
		fields["motion_segments_uploaded"] = strconv.FormatUint(uint64(*d.MotionSegmentsUploaded), 10)
	}
	if d.MotionSegmentsSkipped != nil {
		fields["motion_segments_skipped"] = strconv.FormatUint(uint64(*d.MotionSegmentsSkipped), 10)
	}
	// --- performance health metrics (PR A of the 2026-05-12 perf series) ---
	if d.SegmentUploadP95Ms != nil {
		fields["segment_upload_p95_ms"] = strconv.FormatUint(uint64(*d.SegmentUploadP95Ms), 10)
	}
	if d.SegmentUploadRetries != nil {
		fields["segment_upload_retries"] = strconv.FormatUint(uint64(*d.SegmentUploadRetries), 10)
	}
	if d.SegmentQueueDepth != nil {
		fields["segment_queue_depth"] = strconv.FormatUint(uint64(*d.SegmentQueueDepth), 10)
	}
	if d.LiveWSBytesPerSec != nil {
		fields["live_ws_bytes_per_sec"] = strconv.FormatUint(uint64(*d.LiveWSBytesPerSec), 10)
	}
	if d.LiveWSDroppedFrames != nil {
		fields["live_ws_dropped_frames"] = strconv.FormatUint(uint64(*d.LiveWSDroppedFrames), 10)
	}
	if d.GpsdQueryMs != nil {
		fields["gpsd_query_ms"] = strconv.FormatUint(uint64(*d.GpsdQueryMs), 10)
	}
	if d.EventLoopLagMs != nil {
		fields["event_loop_lag_ms"] = strconv.FormatUint(uint64(*d.EventLoopLagMs), 10)
	}
	if d.DiskUsedPct != nil {
		fields["disk_used_pct"] = strconv.FormatUint(uint64(*d.DiskUsedPct), 10)
	}
	if d.ModemRAT != nil {
		fields["modem_rat"] = *d.ModemRAT
	}
	if d.NetworkRecoveryAttempts != nil {
		fields["network_recovery_attempts"] = strconv.FormatUint(uint64(*d.NetworkRecoveryAttempts), 10)
	}
	return fields
}

// FieldsToEntry parses Redis stream entry fields into a TelemetryEntry.
func FieldsToEntry(fields map[string]interface{}) (*apitypes.TelemetryEntry, error) {
	e := &apitypes.TelemetryEntry{}
	for k, v := range fields {
		s, ok := v.(string)
		if !ok {
			continue
		}
		switch k {
		case "ts":
			n, _ := strconv.ParseUint(s, 10, 64)
			e.TS = n
		case "server_ts":
			n, _ := strconv.ParseUint(s, 10, 64)
			e.ServerTS = n
		case "sig":
			n, _ := strconv.ParseInt(s, 10, 8)
			v := int8(n)
			e.Sig = &v
		case "temp":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.Temp = &v
		case "fps":
			n, _ := strconv.ParseFloat(s, 32)
			v := float32(n)
			e.FPS = &v
		case "kbps":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.Kbps = &v
		case "cpu":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.CPU = &v
		case "mem":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.Mem = &v
		case "uptime":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.Uptime = &v
		case "lat":
			n, _ := strconv.ParseFloat(s, 64)
			e.Lat = &n
		case "lon":
			n, _ := strconv.ParseFloat(s, 64)
			e.Lon = &n
		case "alt":
			n, _ := strconv.ParseFloat(s, 32)
			v := float32(n)
			e.Alt = &v
		case "gps_fix":
			n, _ := strconv.ParseUint(s, 10, 8)
			v := uint8(n)
			e.GPSFix = &v
		case "power_mode":
			v := s
			e.PowerMode = &v
		case "upload_mode":
			v := s
			e.UploadMode = &v
		case "battery_pct":
			n, _ := strconv.ParseUint(s, 10, 8)
			v := uint8(n)
			e.BatteryPct = &v
		case "motion_segments_uploaded":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.MotionSegmentsUploaded = &v
		case "motion_segments_skipped":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.MotionSegmentsSkipped = &v
		case "segment_upload_p95_ms":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.SegmentUploadP95Ms = &v
		case "segment_upload_retries":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.SegmentUploadRetries = &v
		case "segment_queue_depth":
			n, _ := strconv.ParseUint(s, 10, 8)
			v := uint8(n)
			e.SegmentQueueDepth = &v
		case "live_ws_bytes_per_sec":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.LiveWSBytesPerSec = &v
		case "live_ws_dropped_frames":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.LiveWSDroppedFrames = &v
		case "gpsd_query_ms":
			n, _ := strconv.ParseUint(s, 10, 16)
			v := uint16(n)
			e.GpsdQueryMs = &v
		case "event_loop_lag_ms":
			n, _ := strconv.ParseUint(s, 10, 16)
			v := uint16(n)
			e.EventLoopLagMs = &v
		case "disk_used_pct":
			n, _ := strconv.ParseUint(s, 10, 8)
			v := uint8(n)
			e.DiskUsedPct = &v
		case "modem_rat":
			v := s
			e.ModemRAT = &v
		case "network_recovery_attempts":
			n, _ := strconv.ParseUint(s, 10, 32)
			v := uint32(n)
			e.NetworkRecoveryAttempts = &v
		}
	}
	return e, nil
}

// QueryTelemetryRange returns telemetry entries for a device between fromMs and toMs.
func QueryTelemetryRange(ctx context.Context, rdb *goredis.Client, deviceID string, fromMs, toMs uint64, limit int64) ([]apitypes.TelemetryEntry, error) {
	if rdb == nil {
		return nil, nil
	}
	key := telemetryKeyPrefix + deviceID
	if limit <= 0 || limit > 1000 {
		limit = 600
	}

	results, err := rdb.XRangeN(ctx, key, fmt.Sprintf("%d", fromMs), fmt.Sprintf("%d", toMs), limit).Result()
	if err != nil {
		return nil, err
	}

	entries := make([]apitypes.TelemetryEntry, 0, len(results))
	for _, msg := range results {
		e, err := FieldsToEntry(msg.Values)
		if err != nil {
			continue
		}
		entries = append(entries, *e)
	}
	return entries, nil
}

// QueryTelemetryLatest returns the most recent telemetry entry for a device.
func QueryTelemetryLatest(ctx context.Context, rdb *goredis.Client, deviceID string) (*apitypes.TelemetryEntry, error) {
	if rdb == nil {
		return nil, nil
	}
	key := telemetryKeyPrefix + deviceID

	results, err := rdb.XRevRangeN(ctx, key, "+", "-", 1).Result()
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}

	return FieldsToEntry(results[0].Values)
}
