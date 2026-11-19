package tantei

import (
	"errors"
	"time"
)

// DecodeDateTimeOffsetBinary reads EF Core's binary storage format, not
// DateTime.ToBinary: high bits are local ticks in 100-microsecond units,
// and the low 11 bits are a signed UTC offset in minutes.
func DecodeDateTimeOffsetBinary(value int64) (time.Time, error) {
	const epochSeconds int64 = 62135596800
	const unitsPerSecond int64 = 10000
	const maxLocalUnits int64 = 3155378975999999
	units := value >> 11
	offset := value & 0x7ff
	if offset >= 1024 {
		offset -= 2048
	}
	if value < 0 || units > maxLocalUnits || offset < -840 || offset > 840 {
		return time.Time{}, errors.New("invalid .NET DateTimeOffset binary timestamp")
	}
	utcUnits := units - epochSeconds*unitsPerSecond - offset*60*unitsPerSecond
	decoded := time.Unix(utcUnits/unitsPerSecond, (utcUnits%unitsPerSecond)*100000).UTC()
	if decoded.Year() < 1 || decoded.Year() > 9999 {
		return time.Time{}, errors.New(".NET DateTimeOffset UTC time is outside its valid range")
	}
	return decoded, nil
}
