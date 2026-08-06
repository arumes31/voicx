// Package safecast provides checked conversions between integer types.
package safecast

import (
	"errors"
	"fmt"
	"math"
)

// ErrOutOfRange reports that a value cannot be represented by the target type.
var ErrOutOfRange = errors.New("safecast: value out of range")

// Int64ToUint32 converts value to uint32 when it is representable.
func Int64ToUint32(value int64) (uint32, error) {
	if value < 0 || value > int64(math.MaxUint32) {
		return 0, outOfRange(value, "uint32")
	}
	return uint32(value), nil
}

// Int64ToUint16 converts value to uint16 when it is representable.
func Int64ToUint16(value int64) (uint16, error) {
	if value < 0 || value > int64(math.MaxUint16) {
		return 0, outOfRange(value, "uint16")
	}
	return uint16(value), nil
}

// IntToInt32 converts value to int32 when it is representable.
func IntToInt32(value int) (int32, error) {
	wide := int64(value)
	if wide < math.MinInt32 || wide > math.MaxInt32 {
		return 0, outOfRange(value, "int32")
	}
	return int32(value), nil
}

// IntToUint32 converts value to uint32 when it is representable.
func IntToUint32(value int) (uint32, error) {
	return Int64ToUint32(int64(value))
}

// IntToUint8 converts value to uint8 when it is representable.
func IntToUint8(value int) (uint8, error) {
	if value < 0 || value > math.MaxUint8 {
		return 0, outOfRange(value, "uint8")
	}
	return uint8(value), nil
}

// Uint32ToInt converts value to int when it is representable on this platform.
func Uint32ToInt(value uint32) (int, error) {
	if uint64(value) > uint64(math.MaxInt) {
		return 0, outOfRange(value, "int")
	}
	return int(value), nil
}

func outOfRange(value any, target string) error {
	return fmt.Errorf("%w: %v cannot be represented as %s", ErrOutOfRange, value, target)
}
