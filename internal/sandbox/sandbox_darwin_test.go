//go:build darwin

package sandbox

import "testing"

// On macOS a grant changes nothing in the profile: GPU access is open to every confined
// command, so a grant must not be read as what keeps others off it.
func TestDevicesChangeNothingOnMacOS(t *testing.T) {
	base := Policy{Read: []string{"/r"}, Write: []string{"/w"}, Temp: "/t"}
	granted := base
	granted.Devices = []string{"/dev/nvidia0"}
	granted.VisibleGPUs = []string{"GPU-a"}
	if Profile(base) != Profile(granted) {
		t.Fatal("a device grant changed the profile")
	}
}
