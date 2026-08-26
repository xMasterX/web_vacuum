package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that round-trips through YAML and JSON as a
// human-readable string ("30s", "2m500ms"). Bare numbers are read as seconds so
// hand-written config and web-form input both work.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// ParseDuration reads a duration the way a person would write one.
//
// Go's own parser accepts "5s" and rejects "5", "5 sec" and "5 seconds", which
// is a poor thing to enforce on someone typing into a settings field. Anything
// unambiguous is accepted here, and a bare number means seconds.
func ParseDuration(s string) (Duration, error) { return parseDuration(s) }

var durationWords = strings.NewReplacer(
	" ", "",
	"milliseconds", "ms", "millisecond", "ms", "millis", "ms", "milli", "ms", "msec", "ms",
	"seconds", "s", "second", "s", "secs", "s", "sec", "s",
	"minutes", "m", "minute", "m", "mins", "m", "min", "m",
	"hours", "h", "hour", "h", "hrs", "h", "hr", "h",
)

func parseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if v, err := time.ParseDuration(s); err == nil {
		return Duration(v), nil
	}
	// A bare number is the most common thing to type, and seconds is the only
	// unit it could sensibly mean here.
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return Duration(time.Duration(f * float64(time.Second))), nil
	}
	// "5 seconds", "2 min", "1 hr" and friends.
	normalized := durationWords.Replace(strings.ToLower(s))
	if v, err := time.ParseDuration(normalized); err == nil {
		return Duration(v), nil
	}
	if f, err := strconv.ParseFloat(normalized, 64); err == nil {
		return Duration(time.Duration(f * float64(time.Second))), nil
	}
	return 0, fmt.Errorf("could not read %q as a length of time — try 5s, 500ms or 2m", s)
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case string:
		p, err := parseDuration(v)
		if err != nil {
			return err
		}
		*d = p
	case float64:
		*d = Duration(time.Duration(v * float64(time.Second)))
	case nil:
		*d = 0
	default:
		return fmt.Errorf("invalid duration %v", raw)
	}
	return nil
}

// ByteSize is a byte count that accepts "10MB", "512k" or a bare number.
type ByteSize int64

func (b ByteSize) V() int64 { return int64(b) }

var byteUnits = []struct {
	suffix string
	mult   int64
}{
	{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
	{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
}

func (b ByteSize) String() string {
	n := int64(b)
	if n == 0 {
		return "0"
	}
	for _, u := range byteUnits {
		if u.mult > 1 && n%u.mult == 0 && n >= u.mult {
			return strconv.FormatInt(n/u.mult, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

func parseByteSize(s string) (ByteSize, error) {
	if s == "" {
		return 0, nil
	}
	up := ""
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r -= 32
		}
		if r != ' ' {
			up += string(r)
		}
	}
	for _, u := range byteUnits {
		if len(up) > len(u.suffix) && up[len(up)-len(u.suffix):] == u.suffix {
			f, err := strconv.ParseFloat(up[:len(up)-len(u.suffix)], 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return ByteSize(int64(f * float64(u.mult))), nil
		}
	}
	n, err := strconv.ParseInt(up, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return ByteSize(n), nil
}

func (b ByteSize) MarshalYAML() (any, error) { return b.String(), nil }

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := parseByteSize(s)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

func (b ByteSize) MarshalJSON() ([]byte, error) { return json.Marshal(b.String()) }

func (b *ByteSize) UnmarshalJSON(raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case string:
		p, err := parseByteSize(t)
		if err != nil {
			return err
		}
		*b = p
	case float64:
		*b = ByteSize(int64(t))
	case nil:
		*b = 0
	default:
		return fmt.Errorf("invalid size %v", v)
	}
	return nil
}
