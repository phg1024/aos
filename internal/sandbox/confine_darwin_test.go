//go:build darwin

package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"aos/internal/sandbox"
)

// What macOS does about the gaps the Linux filter closes, measured, not assumed.
func TestWhatTheMacOSProfileRefuses(t *testing.T) {
	r := newConfined(t)
	dir := shortDir(t)
	stream := filepath.Join(dir, "stream")
	listen(t, "unix", stream)
	tcp := tcpListener(t)
	udp, arrived := udpListener(t)
	network := withNetworkMac(r.policy)

	v := r.probe(r.policy, "unix", stream)
	t.Logf("no network: unix path: %s", v)
	if v == "open" {
		t.Errorf("a Unix socket with no network: %s", v)
	}
	v = r.probe(r.policy, "tcp", tcp)
	t.Logf("no network: tcp: %s", v)
	if v == "open" {
		t.Errorf("TCP with no network: %s", v)
	}
	v = r.probe(r.policy, "udp", udp)
	got := arrived()
	t.Logf("no network: udp: %s, arrived %v", v, got)
	if v == "open" || got {
		t.Errorf("UDP with no network: %s", v)
	}
	v = r.probe(network, "unix", stream)
	t.Logf("network: unix path: %s", v)
	v = r.probe(network, "tcp", tcp)
	t.Logf("network: tcp: %s", v)

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[1])
	inherited := os.NewFile(uintptr(fds[0]), "inherited")
	defer inherited.Close()
	cmd, err := sandbox.Command(context.Background(), r.policy, r.ws, `if ( : >&3 ) 2>/dev/null; then echo survived; else echo closed; fi`)
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = []*os.File{inherited}
	out, err := cmd.CombinedOutput()
	t.Logf("inherited socket: %s %v", strings.TrimSpace(string(out)), err)
}

// launchctl stays out of reach, even for a policy that grants its directory. The submit
// would be refused by launchd regardless; the marker says nothing ran outside.
func TestLaunchctlIsOutOfReach(t *testing.T) {
	r := newConfined(t)
	wide := r.policy
	wide.Read = append(append([]string{}, r.policy.Read...), "/bin")
	marker := filepath.Join(shortDir(t), "marker")
	label := fmt.Sprintf("aos.sandbox.test.%d", os.Getpid())
	t.Cleanup(func() { exec.Command("/bin/launchctl", "remove", label).Run() })
	for _, p := range []sandbox.Policy{r.policy, wide, withNetworkMac(r.policy)} {
		for _, command := range []string{
			"/bin/launchctl print gui/$(id -u)",
			"/bin/launchctl submit -l " + label + " -- /bin/sh -c 'id > " + marker + "; sleep 60'",
		} {
			if out, err := r.shell(p, command); err == nil || !strings.Contains(out, "Operation not permitted") {
				t.Errorf("%q: %v %q", command, err, out)
			}
		}
		if out, err := r.shell(p, "cp /bin/launchctl copy"); err == nil {
			t.Errorf("launchctl could be copied: %q", out)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a submitted job ran outside the sandbox")
	}
}

func withNetworkMac(p sandbox.Policy) sandbox.Policy { p.Network = true; return p }
