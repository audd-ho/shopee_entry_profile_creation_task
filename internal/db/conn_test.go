package db

import (
	"database/sql/driver"
	"math"
	"testing"
	"time"
)

// These are pure unit tests for the database/sql replacement — the hop from
// driver.Value to typed struct fields. No MySQL required.

func TestScanValue(t *testing.T) {
	var (
		s   string
		b   []byte
		u64 uint64
		u32 uint32
		i64 int64
		tm  time.Time
	)

	tests := []struct {
		name  string
		src   driver.Value
		dest  any
		check func() bool
		want  string
	}{
		{"string from []byte", []byte("alice"), &s, func() bool { return s == "alice" }, `"alice"`},
		{"string from string", "alice", &s, func() bool { return s == "alice" }, `"alice"`},
		// NULL becomes the zero value. This is what the sql.NullString dance
		// around avatar_suffix amounted to.
		{"string from NULL", nil, &s, func() bool { return s == "" }, `""`},

		{"bytes from []byte", []byte{1, 2, 3}, &b, func() bool { return len(b) == 3 && b[0] == 1 }, "[1 2 3]"},
		{"bytes from NULL", nil, &b, func() bool { return b == nil }, "nil"},

		// An unsigned column may arrive as either, depending on the driver's
		// binary row decoder, so both are accepted.
		{"uint64 from uint64", uint64(42), &u64, func() bool { return u64 == 42 }, "42"},
		{"uint64 from int64", int64(42), &u64, func() bool { return u64 == 42 }, "42"},
		{"uint64 from NULL", nil, &u64, func() bool { return u64 == 0 }, "0"},

		{"uint32 from int64", int64(7), &u32, func() bool { return u32 == 7 }, "7"},
		{"int64 from int64", int64(-7), &i64, func() bool { return i64 == -7 }, "-7"},

		{"time from time", time.Unix(0, 0).UTC(), &tm, func() bool { return tm.Equal(time.Unix(0, 0).UTC()) }, "epoch"},
		{"time from NULL", nil, &tm, func() bool { return tm.IsZero() }, "zero"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := scanValue(tt.src, tt.dest); err != nil {
				t.Fatalf("scanValue(%v) = %v", tt.src, err)
			}
			if !tt.check() {
				t.Errorf("scanValue(%v) did not produce %s", tt.src, tt.want)
			}
		})
	}
}

func TestScanValueRejects(t *testing.T) {
	var u32 uint32
	var u64 uint64
	var s string
	var unsupported complex128

	tests := []struct {
		name string
		src  driver.Value
		dest any
	}{
		{"uint32 overflow", int64(math.MaxUint32) + 1, &u32},
		{"negative into unsigned", int64(-1), &u64},
		{"wrong source type for string", 3.14, &s},
		{"unsupported destination", int64(1), &unsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := scanValue(tt.src, tt.dest); err == nil {
				t.Errorf("scanValue(%v, %T) = nil, want an error", tt.src, tt.dest)
			}
		})
	}
}

func TestRowScanArityMismatch(t *testing.T) {
	r := &Row{vals: []driver.Value{int64(1), int64(2)}}
	var one uint64
	if err := r.Scan(&one); err == nil {
		t.Error("Scan with 1 destination for a 2-column row = nil, want an error")
	}
}

// TestCloneValuesCopiesBytes is the safety net for the aliasing problem. The
// driver's []byte points into the connection's read buffer, whose lifetime is
// the connection's, not the row's. If this ever stopped copying, one user's
// data could surface in another's response once the connection was reused.
func TestCloneValuesCopiesBytes(t *testing.T) {
	buffer := []byte("alice")
	cloned := cloneValues([]driver.Value{buffer, int64(1), nil})

	got, ok := cloned[0].([]byte)
	if !ok {
		t.Fatalf("cloned[0] is %T, want []byte", cloned[0])
	}
	if string(got) != "alice" {
		t.Fatalf("cloned value = %q, want %q", got, "alice")
	}

	// Simulate the driver reusing its buffer for the next row.
	copy(buffer, "bobby")

	if string(got) != "alice" {
		t.Errorf("clone changed to %q when the source buffer was overwritten; "+
			"the driver's bytes were aliased, not copied", got)
	}
	// Non-byte values pass through untouched.
	if cloned[1] != int64(1) || cloned[2] != nil {
		t.Errorf("cloneValues altered non-byte values: %v", cloned)
	}
}

// TestScanStringCopiesFromBuffer is the same property one level up: converting
// []byte to string must not retain the driver's array.
func TestScanStringCopiesFromBuffer(t *testing.T) {
	buffer := []byte("alice")

	var s string
	if err := scanValue(buffer, &s); err != nil {
		t.Fatalf("scanValue: %v", err)
	}

	copy(buffer, "bobby")

	if s != "alice" {
		t.Errorf("scanned string became %q after the buffer was overwritten", s)
	}
}

func TestToNamedValues(t *testing.T) {
	args := []any{"alice", []byte{1, 2}, uint64(42), 7}

	got, err := toNamedValues(args)
	if err != nil {
		t.Fatalf("toNamedValues: %v", err)
	}
	if len(got) != len(args) {
		t.Fatalf("got %d values, want %d", len(got), len(args))
	}

	// Ordinals are 1-based, which is what the driver expects for ? parameters.
	for i, nv := range got {
		if nv.Ordinal != i+1 {
			t.Errorf("value %d has Ordinal %d, want %d", i, nv.Ordinal, i+1)
		}
	}
	// uint64 must be narrowed: driver.Value does not include unsigned types.
	if v, ok := got[2].Value.(int64); !ok || v != 42 {
		t.Errorf("uint64 argument became %#v, want int64(42)", got[2].Value)
	}
	if v, ok := got[3].Value.(int64); !ok || v != 7 {
		t.Errorf("int argument became %#v, want int64(7)", got[3].Value)
	}
}

func TestToNamedValuesRejects(t *testing.T) {
	if _, err := toNamedValues([]any{uint64(math.MaxUint64)}); err == nil {
		t.Error("a uint64 that overflows int64 was accepted; it would silently wrap")
	}
	if _, err := toNamedValues([]any{struct{}{}}); err == nil {
		t.Error("an unsupported argument type was accepted")
	}
}
