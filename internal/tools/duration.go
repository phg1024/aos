package tools

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ErrBadDuration marks a duration arg no tool should act on.
var ErrBadDuration = errors.New("bad duration")

// DurationArg reads a tool's optional duration arg as a model writes it: a whole
// number of seconds, or a Go duration such as 5m. Empty means fallback. Anything
// else, zero and negatives included, is an error the model reads, so it never
// believes it asked for a time the tool did not use. name is how the error names
// the arg. Each tool decides what a value above its own limit means.
func DurationArg(name, raw string, fallback time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	refuse := func(why string) (time.Duration, error) {
		return 0, fmt.Errorf("%w: %s %q %s; give a whole number of seconds, or a Go duration such as 5m", ErrBadDuration, name, raw, why)
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		seconds, parseErr := strconv.ParseInt(raw, 10, 64)
		switch {
		case errors.Is(parseErr, strconv.ErrRange), parseErr == nil && seconds > math.MaxInt64/int64(time.Second):
			return refuse("is too long")
		case parseErr != nil:
			return refuse("cannot be read")
		}
		value = time.Duration(seconds) * time.Second
	}
	if value <= 0 {
		return refuse("is not above zero")
	}
	return value, nil
}
