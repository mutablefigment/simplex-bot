package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ByteSize is a byte count that unmarshals from a human-readable string such as
// "25MB", "100MiB" or "1GB". It mirrors Duration: a named scalar with an
// UnmarshalText so TOML values can be written for humans, stored as an int64.
//
// Unit convention:
//   - SI / decimal suffixes use powers of 1000: KB=10^3, MB=10^6, GB=10^9, TB=10^12.
//   - IEC / binary suffixes use powers of 1024:  KiB=2^10, MiB=2^20, GiB=2^30, TiB=2^40.
//   - A bare number, or one suffixed with "B"/"" , is a raw byte count.
//
// Suffix matching is case-insensitive and tolerant of surrounding/internal
// whitespace (e.g. "100 MiB"). The numeric part may be fractional ("1.5GB");
// the result is truncated toward zero to whole bytes. Negative values are
// rejected.
type ByteSize int64

// byteUnits is ordered longest-suffix-first so that, after upper-casing, "KIB"
// is tested before "KB" and "B" is tested last (it is a suffix of every other
// unit). Sizes are expressed as plain int64 byte multipliers.
var byteUnits = []struct {
	suffix string
	mult   int64
}{
	{"KIB", 1 << 10},
	{"MIB", 1 << 20},
	{"GIB", 1 << 30},
	{"TIB", 1 << 40},
	{"KB", 1000},
	{"MB", 1000 * 1000},
	{"GB", 1000 * 1000 * 1000},
	{"TB", 1000 * 1000 * 1000 * 1000},
	{"B", 1},
}

func (s *ByteSize) UnmarshalText(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "" {
		return fmt.Errorf("empty byte size")
	}
	upper := strings.ToUpper(raw)

	num := upper
	var mult int64 = 1
	for _, u := range byteUnits {
		if strings.HasSuffix(upper, u.suffix) {
			num = upper[:len(upper)-len(u.suffix)]
			mult = u.mult
			break
		}
	}
	// Tolerate a single gap between the number and its unit ("100 MiB") by
	// trimming trailing space off the numeric segment. Embedded spaces inside
	// the number itself (e.g. "10 20MB") survive here and are rejected by
	// ParseFloat below.
	num = strings.TrimSpace(num)
	if num == "" {
		return fmt.Errorf("byte size %q: missing number", raw)
	}

	val, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return fmt.Errorf("byte size %q: invalid number %q", raw, num)
	}
	if val < 0 {
		return fmt.Errorf("byte size %q: must not be negative", raw)
	}
	*s = ByteSize(int64(val * float64(mult)))
	return nil
}
