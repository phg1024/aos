//go:build linux

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
	"unsafe"

	"aos/internal/sandbox"
)

func init() {
	raw := func(number uintptr, a, b, c uintptr) error {
		fd, _, errno := syscall.Syscall(number, a, b, c)
		if errno != 0 {
			return errno
		}
		syscall.Close(int(fd))
		return nil
	}
	pair := func(kind int) error {
		fds, err := syscall.Socketpair(syscall.AF_UNIX, kind, 0)
		if err != nil {
			return err
		}
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		return nil
	}
	probes["seqpacket"] = func(args []string) error { return dialUnix(syscall.SOCK_SEQPACKET, args[0]) }
	probes["netlink"] = func([]string) error { return raw(syscall.SYS_SOCKET, syscall.AF_NETLINK, syscall.SOCK_RAW, 0) }
	probes["vsock"] = func([]string) error { return raw(syscall.SYS_SOCKET, 40, syscall.SOCK_STREAM, 0) }
	probes["uring"] = func([]string) error {
		params := make([]byte, 120)
		return raw(425, 2, uintptr(unsafe.Pointer(&params[0])), 0)
	}
	probes["streampair"] = func([]string) error { return pair(syscall.SOCK_STREAM) }
	probes["seqpair"] = func([]string) error { return pair(syscall.SOCK_SEQPACKET) }
	probes["dgrampair"] = func([]string) error { return pair(syscall.SOCK_DGRAM) }
	// What CUDA does when it starts: its own abstract seqpacket listener.
	probes["ownabstract"] = func(args []string) error {
		fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
		if err != nil {
			return fmt.Errorf("socket: %w", err)
		}
		if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: "@" + args[0]}); err != nil {
			return fmt.Errorf("bind: %w", err)
		}
		if err := syscall.Listen(fd, 8); err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		return dialUnix(syscall.SOCK_SEQPACKET, "@"+args[0])
	}
	// This stands in for a kernel or a container that refuses a filter: it takes the
	// call away, then starts the helper.
	probes["noseccomp"] = func(args []string) error {
		program := []struct {
			code   uint16
			jt, jf uint8
			k      uint32
		}{
			{0x20, 0, 0, 0},                         // load the syscall number
			{0x15, 0, 3, uint32(syscall.SYS_PRCTL)}, // not prctl: allow
			{0x20, 0, 0, 16},                        // load the first argument
			{0x15, 0, 1, 22},                        // not PR_SET_SECCOMP: allow
			{0x06, 0, 0, 0x00050000 | 1},            // EPERM
			{0x06, 0, 0, 0x7fff0000},                // allow
		}
		header := struct {
			length uint16
			filter unsafe.Pointer
		}{uint16(len(program)), unsafe.Pointer(&program[0])}
		if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0); errno != 0 {
			return errno
		}
		if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, 22, 2, uintptr(unsafe.Pointer(&header))); errno != 0 {
			return errno
		}
		return syscall.Exec(args[0], args, os.Environ())
	}
}

func open(verdict string) bool { return verdict == "open" }

// What each kind of policy may reach, through the real helper. The listeners are all
// outside the sandbox and outside every grant.
func TestSocketsAreGrantedByPolicy(t *testing.T) {
	r := newConfined(t)
	dir := shortDir(t)
	stream, seq := filepath.Join(dir, "stream"), filepath.Join(dir, "seq")
	suffix := fmt.Sprint(os.Getpid())
	listen(t, "unix", stream)
	listen(t, "unixpacket", seq)
	listen(t, "unix", "@aos-test-stream-"+suffix)
	listen(t, "unixpacket", "@aos-test-seq-"+suffix)
	tcp := tcpListener(t)
	udp, arrived := udpListener(t)

	plain := r.policy
	network := r.policy
	network.Network = true
	gpu := r.policy
	gpu.Devices = []string{"/dev/null"}

	cases := []struct {
		probe            string
		arg              string
		plain, net, card bool
	}{
		{"unix", stream, false, false, false},
		{"unix", "@aos-test-stream-" + suffix, false, false, false},
		{"seqpacket", stream, false, false, false},
		// A listener of the type a device grant may make, at a path: the one thing it
		// opens, and reported as a remaining gap.
		{"seqpacket", seq, false, false, true},
		{"seqpacket", "@aos-test-seq-" + suffix, false, false, false},
		{"ownabstract", "aos-test-own-" + suffix, false, false, true},
		{"tcp", tcp, false, true, false},
		{"netlink", "", false, false, false},
		{"vsock", "", false, false, false},
		{"uring", "", false, false, false},
		{"streampair", "", true, true, true},
		{"seqpair", "", true, true, true},
		{"dgrampair", "", false, false, false},
	}
	for _, c := range cases {
		for _, p := range []struct {
			name   string
			policy sandbox.Policy
			want   bool
		}{{"plain", plain, c.plain}, {"network", network, c.net}, {"device grant", gpu, c.card}} {
			args := []string{}
			if c.arg != "" {
				args = append(args, c.arg)
			}
			verdict := r.probe(p.policy, c.probe, args...)
			if !open(verdict) && !strings.HasPrefix(verdict, "refused:") {
				t.Fatalf("%s %s: %q", c.probe, c.arg, verdict)
			}
			if open(verdict) != p.want {
				t.Errorf("%s: %s %s: %q", p.name, c.probe, c.arg, verdict)
			}
		}
	}
	if v := r.probe(plain, "udp", udp); open(v) || arrived() {
		t.Errorf("UDP went out with no network: %q", v)
	}
	if v := r.probe(gpu, "udp", udp); open(v) || arrived() {
		t.Errorf("UDP went out under a device grant: %q", v)
	}
	if v := r.probe(network, "udp", udp); !open(v) || !arrived() {
		t.Errorf("UDP with network: %q", v)
	}
}

// What hands the service manager over, with no help from the shell's other tools.
func TestTheServiceManagerIsOutOfReach(t *testing.T) {
	r := newConfined(t)
	runtimeDir := fmt.Sprintf("/run/user/%d", os.Getuid())
	if _, err := os.Stat(filepath.Join(runtimeDir, "systemd/private")); err != nil {
		t.Skip("no user service manager here")
	}
	for _, path := range []string{"bus", "systemd/private"} {
		for _, p := range []sandbox.Policy{r.policy, withNetwork(r.policy), withDevice(r.policy)} {
			full := filepath.Join(runtimeDir, path)
			if v := r.probe(p, "unix", full); open(v) {
				t.Errorf("%s by stream: %+v", full, p)
			}
			if v := r.probe(p, "seqpacket", full); open(v) {
				t.Errorf("%s by seqpacket: %+v", full, p)
			}
		}
	}
}

func withNetwork(p sandbox.Policy) sandbox.Policy { p.Network = true; return p }
func withDevice(p sandbox.Policy) sandbox.Policy  { p.Devices = []string{"/dev/null"}; return p }

// aos does not choose what it inherits. A socket it was started with must not reach
// the command, whether or not it is close-on-exec.
func TestAnInheritedSocketDoesNotSurvive(t *testing.T) {
	r := newConfined(t)
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[1])
	inherited := os.NewFile(uintptr(fds[0]), "inherited")
	defer inherited.Close()
	cmd, err := sandbox.Command(context.Background(), r.policy, r.ws, `if [ -e /proc/$$/fd/3 ]; then echo survived; else echo closed; fi`)
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = []*os.File{inherited}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "closed" {
		t.Fatalf("%q %v", out, err)
	}
}

// When the filter cannot go on, the command does not run at all.
func TestARunWithoutItsFilterIsRefused(t *testing.T) {
	r := newConfined(t)
	marker := filepath.Join(r.ws, "ran")
	cmd, err := sandbox.Command(context.Background(), r.policy, r.ws, "echo ran > "+marker)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := exec.Command(r.self, append([]string{probeArg, "noseccomp", cmd.Path}, cmd.Args[1:]...)...)
	wrapped.Dir, wrapped.Env = cmd.Dir, cmd.Env
	out, err := wrapped.CombinedOutput()
	var exit *exec.ExitError
	if !asExit(err, &exit) || exit.ExitCode() != 126 || !strings.Contains(string(out), "seccomp socket filter") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran without its filter")
	}
}

func asExit(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError)
	if ok {
		*target = exit
	}
	return ok
}
