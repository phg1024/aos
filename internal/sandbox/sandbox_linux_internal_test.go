//go:build linux

package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// Below ABI 6 an ordinary command runs and is told what is open. A device grant does
// not run, since the abstract scope is what makes its seqpacket allowance narrow.
func TestOlderKernelsAndTheScopes(t *testing.T) {
	plain := Policy{Temp: "/t"}
	grant := Policy{Temp: "/t", Devices: []string{"/dev/nvidia0"}}
	if err := checkABI(5, plain); err != nil {
		t.Errorf("ABI 5, no grant: %v", err)
	}
	if err := checkABI(5, grant); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ABI 5, a device grant: %v", err)
	}
	if err := checkABI(6, grant); err != nil {
		t.Errorf("ABI 6, a device grant: %v", err)
	}
	if warning := scopeWarning(5); !strings.Contains(warning, "signal") {
		t.Errorf("ABI 5 warning: %q", warning)
	}
	if warning := scopeWarning(6); warning != "" {
		t.Errorf("ABI 6 warning: %q", warning)
	}
}
