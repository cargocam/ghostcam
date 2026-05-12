package redis

import (
	"reflect"
	"testing"

	"github.com/cargocam/ghostcam/common"
)

func TestDatagramToFields_RoundTripsAllOptionalFields(t *testing.T) {
	// Populate every optional pointer field on TelemetryDatagram via
	// reflection. If a new field is added to common/telemetry.go this
	// test exercises it for free — exactly the invariant the
	// reflection-based serializer is supposed to give us.
	d := &common.TelemetryDatagram{TS: 1234567890}
	v := reflect.ValueOf(d).Elem()
	t2 := v.Type()
	for i := 0; i < t2.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Pointer {
			continue
		}
		// Allocate a fresh pointer to the element type, set a sentinel
		// value so we can assert on it after the round-trip.
		f.Set(reflect.New(f.Type().Elem()))
		setSentinel(f.Elem())
	}

	fields := datagramToFields(d, 9876543210)
	entry, err := FieldsToEntry(fields)
	if err != nil {
		t.Fatalf("FieldsToEntry: %v", err)
	}

	// ts + server_ts are always present and non-pointer.
	if entry.TS != 1234567890 {
		t.Errorf("TS = %d, want 1234567890", entry.TS)
	}
	if entry.ServerTS != 9876543210 {
		t.Errorf("ServerTS = %d, want 9876543210", entry.ServerTS)
	}

	// Every TelemetryEntry pointer field with a matching json key on
	// the source datagram must be populated. Done generically so a new
	// field on either side surfaces here.
	ev := reflect.ValueOf(entry).Elem()
	et := ev.Type()
	dgFields := make(map[string]bool)
	for i := 0; i < t2.NumField(); i++ {
		if k := jsonKey(t2.Field(i)); k != "" {
			dgFields[k] = true
		}
	}
	for i := 0; i < et.NumField(); i++ {
		f := et.Field(i)
		key := jsonKey(f)
		if key == "" || key == "ts" || key == "server_ts" {
			continue
		}
		if !dgFields[key] {
			continue // entry has a field the datagram doesn't — fine
		}
		fv := ev.Field(i)
		if fv.Kind() == reflect.Pointer && fv.IsNil() {
			t.Errorf("field %s round-tripped to nil; expected sentinel", key)
		}
	}
}

func TestDatagramToFields_NilPointersStayOutOfMap(t *testing.T) {
	d := &common.TelemetryDatagram{TS: 1}
	// Don't set any optional fields — they're all nil.
	fields := datagramToFields(d, 2)
	if _, ok := fields["sig"]; ok {
		t.Error("nil Sig surfaced as a Redis field")
	}
	if _, ok := fields["modem_rat"]; ok {
		t.Error("nil ModemRAT surfaced as a Redis field")
	}
	// ts + server_ts always present.
	if fields["ts"] != "1" || fields["server_ts"] != "2" {
		t.Errorf("ts/server_ts missing or wrong: %v / %v", fields["ts"], fields["server_ts"])
	}
}

func TestFieldsToEntry_IgnoresUnknownKeys(t *testing.T) {
	// A future server version might write a key the current binary
	// doesn't know about. Parsing must not bail.
	fields := map[string]interface{}{
		"ts":               "100",
		"server_ts":        "200",
		"some_future_key":  "future-value",
		"cpu":              "42",
	}
	entry, err := FieldsToEntry(fields)
	if err != nil {
		t.Fatalf("FieldsToEntry returned error on unknown key: %v", err)
	}
	if entry.TS != 100 {
		t.Errorf("TS = %d, want 100", entry.TS)
	}
	if entry.CPU == nil || *entry.CPU != 42 {
		t.Errorf("CPU = %v, want *42", entry.CPU)
	}
}

func TestFieldsToEntry_HandlesMalformedNumeric(t *testing.T) {
	// Garbage in shouldn't crash; field stays at its zero value.
	fields := map[string]interface{}{
		"ts":  "not-a-number",
		"cpu": "also-garbage",
	}
	entry, err := FieldsToEntry(fields)
	if err != nil {
		t.Fatalf("FieldsToEntry: %v", err)
	}
	if entry.TS != 0 {
		t.Errorf("TS = %d, want 0 on parse failure", entry.TS)
	}
}

// setSentinel writes a distinguishable value into a reflect.Value of
// any supported scalar kind. Used by the round-trip test to verify
// each field actually carries data through Redis serialization.
func setSentinel(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("sentinel")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(42)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(42)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(3.14)
	case reflect.Bool:
		v.SetBool(true)
	}
}
