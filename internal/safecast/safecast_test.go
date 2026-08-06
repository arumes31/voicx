package safecast

import (
	"errors"
	"math"
	"strconv"
	"testing"
)

func TestInt64ToUint32(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     int64
		expected  uint32
		shouldErr bool
	}{
		{name: "zero", value: 0, expected: 0},
		{name: "maximum", value: math.MaxUint32, expected: math.MaxUint32},
		{name: "negative", value: -1, shouldErr: true},
		{name: "above maximum", value: int64(math.MaxUint32) + 1, shouldErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Int64ToUint32(test.value)
			assertConversion(t, got, err, test.expected, test.shouldErr)
		})
	}
}

func TestInt64ToUint16(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     int64
		expected  uint16
		shouldErr bool
	}{
		{name: "zero", value: 0, expected: 0},
		{name: "maximum", value: math.MaxUint16, expected: math.MaxUint16},
		{name: "negative", value: -1, shouldErr: true},
		{name: "above maximum", value: int64(math.MaxUint16) + 1, shouldErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Int64ToUint16(test.value)
			assertConversion(t, got, err, test.expected, test.shouldErr)
		})
	}
}

func TestIntToInt32(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name      string
		value     int
		expected  int32
		shouldErr bool
	}
	tests := []testCase{
		{name: "minimum", value: math.MinInt32, expected: math.MinInt32},
		{name: "maximum", value: math.MaxInt32, expected: math.MaxInt32},
	}
	if strconv.IntSize == 64 {
		tests = append(tests,
			testCase{name: "below minimum", value: int(int64(math.MinInt32) - 1), shouldErr: true},
			testCase{name: "above maximum", value: int(int64(math.MaxInt32) + 1), shouldErr: true},
		)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := IntToInt32(test.value)
			assertConversion(t, got, err, test.expected, test.shouldErr)
		})
	}
}

func TestIntToUint32(t *testing.T) {
	t.Parallel()

	got, err := IntToUint32(42)
	assertConversion(t, got, err, uint32(42), false)
	_, err = IntToUint32(-1)
	if !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("IntToUint32(-1) error = %v, want ErrOutOfRange", err)
	}
}

func TestIntToUint8(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     int
		expected  uint8
		shouldErr bool
	}{
		{name: "zero", value: 0, expected: 0},
		{name: "maximum", value: math.MaxUint8, expected: math.MaxUint8},
		{name: "negative", value: -1, shouldErr: true},
		{name: "above maximum", value: math.MaxUint8 + 1, shouldErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := IntToUint8(test.value)
			assertConversion(t, got, err, test.expected, test.shouldErr)
		})
	}
}

func TestUint32ToInt(t *testing.T) {
	t.Parallel()

	got, err := Uint32ToInt(42)
	assertConversion(t, got, err, 42, false)
	if strconv.IntSize == 32 {
		_, err = Uint32ToInt(math.MaxUint32)
		if !errors.Is(err, ErrOutOfRange) {
			t.Fatalf("Uint32ToInt(MaxUint32) error = %v, want ErrOutOfRange", err)
		}
	}
}

func assertConversion[T comparable](t *testing.T, got T, err error, expected T, shouldErr bool) {
	t.Helper()
	if shouldErr {
		if !errors.Is(err, ErrOutOfRange) {
			t.Fatalf("conversion error = %v, want ErrOutOfRange", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("conversion error = %v", err)
	}
	if got != expected {
		t.Fatalf("conversion = %v, want %v", got, expected)
	}
}
