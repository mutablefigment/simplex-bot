package config

import "testing"

func TestByteSizeUnmarshalValid(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		// bare bytes
		{"0", 0},
		{"512", 512},
		{"1024", 1024},
		{"100B", 100},
		{"100b", 100},
		// SI / decimal (powers of 1000)
		{"1KB", 1000},
		{"25MB", 25 * 1000 * 1000},
		{"1GB", 1000 * 1000 * 1000},
		{"1TB", 1000 * 1000 * 1000 * 1000},
		// IEC / binary (powers of 1024)
		{"1KiB", 1024},
		{"100MiB", 100 << 20},
		{"1GiB", 1 << 30},
		{"1TiB", 1 << 40},
		// case-insensitive + internal whitespace
		{"100mib", 100 << 20},
		{"100 MiB", 100 << 20},
		{"  1 gb  ", 1000 * 1000 * 1000},
		// fractional, truncated toward zero
		{"1.5GB", 1500 * 1000 * 1000},
		{"0.5KiB", 512},
	}
	for _, c := range cases {
		var bs ByteSize
		if err := bs.UnmarshalText([]byte(c.in)); err != nil {
			t.Errorf("UnmarshalText(%q) unexpected error: %v", c.in, err)
			continue
		}
		if int64(bs) != c.want {
			t.Errorf("UnmarshalText(%q) = %d, want %d", c.in, int64(bs), c.want)
		}
	}
}

func TestByteSizeUnmarshalInvalid(t *testing.T) {
	cases := []string{
		"",        // empty
		"   ",     // whitespace only
		"MB",      // suffix, no number
		"abc",     // not a number
		"1XB",     // unknown suffix (leaves "1X" as the number)
		"-5MB",    // negative
		"1.2.3MB", // malformed number
		"10 20MB", // two numbers
	}
	for _, in := range cases {
		var bs ByteSize
		if err := bs.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("UnmarshalText(%q) = %d, want error", in, int64(bs))
		}
	}
}
