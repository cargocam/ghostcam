package redis

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
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

// datagramToFields walks d via reflection and serializes every field
// whose `json:"..."` tag is non-empty. Pointer fields are skipped when
// nil; scalar fields are always emitted.
//
// Why reflection: the prior per-field if-fanout was a known footgun —
// every new field on TelemetryDatagram needed three updates (this
// function, FieldsToEntry below, and apitypes.TelemetryEntry) and we
// regularly forgot one. PR #83 fixed a months-long silent-drop bug
// caused by exactly that pattern. This loop is ~50 µs per call (a
// telemetry post happens every 10 s in steady state) so the runtime
// cost is irrelevant.
func datagramToFields(d *common.TelemetryDatagram, serverTS uint64) map[string]interface{} {
	fields := map[string]interface{}{
		"ts":        strconv.FormatUint(d.TS, 10),
		"server_ts": strconv.FormatUint(serverTS, 10),
	}

	v := reflect.ValueOf(d).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		key := jsonKey(t.Field(i))
		if key == "" || key == "ts" {
			continue // ts is the only non-pointer scalar we handle above
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}
		s := formatScalar(fv)
		if s != "" {
			fields[key] = s
		}
	}
	return fields
}

// FieldsToEntry parses Redis stream entry fields into a TelemetryEntry
// via reflection. Mirrors datagramToFields on the read path. Unknown
// keys are ignored — a Redis stream might still carry fields from a
// future schema, and we shouldn't fail to parse the rest.
func FieldsToEntry(fields map[string]interface{}) (*apitypes.TelemetryEntry, error) {
	e := &apitypes.TelemetryEntry{}
	v := reflect.ValueOf(e).Elem()
	t := v.Type()

	byKey := make(map[string]reflect.Value, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		key := jsonKey(t.Field(i))
		if key != "" {
			byKey[key] = v.Field(i)
		}
	}

	for k, raw := range fields {
		s, ok := raw.(string)
		if !ok {
			continue
		}
		dst, ok := byKey[k]
		if !ok {
			continue
		}
		assignScalar(dst, s)
	}
	return e, nil
}

// jsonKey returns the "name" part of a `json:"name,omitempty"` tag, or
// "" when the field is untagged (skip).
func jsonKey(f reflect.StructField) string {
	tag, ok := f.Tag.Lookup("json")
	if !ok || tag == "-" {
		return ""
	}
	if comma := indexComma(tag); comma >= 0 {
		return tag[:comma]
	}
	return tag
}

func indexComma(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			return i
		}
	}
	return -1
}

// formatScalar converts a single reflect.Value to the string form
// Redis stores. Returns "" for unsupported kinds (caller drops them).
func formatScalar(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32:
		return strconv.FormatFloat(v.Float(), 'f', -1, 32)
	case reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	case reflect.Bool:
		if v.Bool() {
			return "1"
		}
		return "0"
	}
	return ""
}

// assignScalar parses the Redis-stored string into the destination
// field's kind. Pointer destinations get a fresh-allocated pointer to
// the parsed value; non-pointer destinations get the value directly.
// Parse errors result in the field being left at its zero value — same
// failure mode as the old hand-written switch.
func assignScalar(dst reflect.Value, s string) {
	switch dst.Kind() {
	case reflect.String:
		dst.SetString(s)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			dst.SetInt(n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			dst.SetUint(n)
		}
	case reflect.Float32, reflect.Float64:
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			dst.SetFloat(n)
		}
	case reflect.Pointer:
		// Allocate a fresh element and recurse onto its target.
		dst.Set(reflect.New(dst.Type().Elem()))
		assignScalar(dst.Elem(), s)
	}
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
