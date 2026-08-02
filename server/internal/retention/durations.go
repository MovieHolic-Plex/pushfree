package retention

import (
	"fmt"
	"time"
)

// parseDurations parses each named duration string. It returns a map keyed by
// the same names with the parsed values, or an error naming the first bad
// field. An empty string parses to zero (a valid "disabled" value) so that
// optional retention windows can be turned off from config.
func parseDurations(in map[string]string) (map[string]time.Duration, error) {
	out := make(map[string]time.Duration, len(in))
	for name, raw := range in {
		if raw == "" {
			out[name] = 0
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("config %q: parse duration %q: %w", name, raw, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("config %q: duration must not be negative, got %q", name, raw)
		}
		out[name] = d
	}
	return out, nil
}

// errNonPositive builds a startup error for a field that must be > 0.
func errNonPositive(name, raw string) error {
	return fmt.Errorf("config %q: must be a positive duration, got %q", name, raw)
}
