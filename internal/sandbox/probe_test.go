//go:build linux || darwin

package sandbox_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"aos/internal/sandbox"
)

// These run the real helper. A confined command re-enters this test binary with
// probeArg to make raw calls, since a shell cannot name a socket type.
const probeArg = "__aos_test_probe"

var probes = map[string]func(args []string) error{
	"unix": func(args []string) error { return dialUnix(syscall.SOCK_STREAM, args[0]) },
	"udp": func(args []string) error {
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
		if err != nil {
			return fmt.Errorf("socket: %w", err)
		}
		defer syscall.Close(fd)
		var port int
		fmt.Sscan(args[0], &port)
		return syscall.Sendto(fd, []byte("x"), 0, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}})
	},
	"tcp": func(args []string) error {
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err != nil {
			return fmt.Errorf("socket: %w", err)
		}
		defer syscall.Close(fd)
		var port int
		fmt.Sscan(args[0], &port)
		return syscall.Connect(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}})
	},
}

func dialUnix(kind int, path string) error {
	fd, err := syscall.Socket(syscall.AF_UNIX, kind, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Connect(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

func init() {
	if len(os.Args) < 3 || os.Args[1] != probeArg {
		return
	}
	runtime.LockOSThread()
	probe, ok := probes[os.Args[2]]
	if !ok {
		fmt.Println("no probe", os.Args[2])
		os.Exit(2)
	}
	if err := probe(os.Args[3:]); err != nil {
		fmt.Println("refused:", err)
		os.Exit(3)
	}
	fmt.Println("open")
	os.Exit(0)
}

type confinedRun struct {
	t      *testing.T
	self   string
	ws     string
	policy sandbox.Policy
}

// newConfined is a policy with nothing granted but a workspace, a scratch directory
// and this binary.
func newConfined(t *testing.T) *confinedRun {
	t.Helper()
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, _ = filepath.EvalSymlinks(self)
	root, _ := filepath.EvalSymlinks(t.TempDir())
	ws, scratch := filepath.Join(root, "ws"), filepath.Join(root, "scratch")
	for _, dir := range []string{ws, scratch} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &confinedRun{t: t, self: self, ws: ws, policy: sandbox.Policy{Read: []string{filepath.Dir(self)}, Write: []string{ws}, Temp: scratch}}
}

func (r *confinedRun) shell(p sandbox.Policy, command string) (string, error) {
	r.t.Helper()
	cmd, err := sandbox.Command(context.Background(), p, r.ws, command)
	if err != nil {
		r.t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// probe runs one probe confined and returns its verdict: "open" or "refused: why".
func (r *confinedRun) probe(p sandbox.Policy, name string, args ...string) string {
	r.t.Helper()
	command := r.self + " " + probeArg + " " + name
	for _, arg := range args {
		command += " '" + arg + "'"
	}
	out, _ := r.shell(p, command)
	return out
}

// shortDir is for socket paths, which have a short limit that TempDir can exceed.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func listen(t *testing.T, network, address string) {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
}

// udpListener returns its port and a check for whether anything arrived.
func udpListener(t *testing.T) (string, func() bool) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return fmt.Sprint(conn.LocalAddr().(*net.UDPAddr).Port), func() bool {
		conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, _, err := conn.ReadFromUDP(make([]byte, 8))
		return err == nil
	}
}

func tcpListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// The key that pays for the model stays out of what the model runs, and out of reach
// through its parent.
func TestKeysDoNotReachACommand(t *testing.T) {
	r := newConfined(t)
	t.Setenv("AOS_LLM_API_KEY", "hunter2-llm")
	t.Setenv("AOS_VLLM_API_KEY", "hunter2-vllm")
	t.Setenv("AOS_KEY_OTHER", "hunter2-other")
	t.Setenv("AOS_TEST_ORDINARY", "still-here")
	out, err := r.shell(r.policy, "env")
	if err != nil {
		t.Fatal(out, err)
	}
	if strings.Contains(out, "hunter2") {
		t.Fatalf("a key reached the command: %s", out)
	}
	if !strings.Contains(out, "AOS_TEST_ORDINARY=still-here") {
		t.Fatalf("an ordinary variable did not: %s", out)
	}
	if runtime.GOOS == "linux" {
		out, err := r.shell(r.policy, "cat /proc/$PPID/environ")
		if err == nil || strings.Contains(out, "hunter2") {
			t.Fatalf("its parent's environment was readable: %v %q", err, out)
		}
	}
}

// A command can signal what it started, and nothing else of this user's.
func TestSignalsStayInside(t *testing.T) {
	r := newConfined(t)
	outside := startSleeper(t)
	if out, err := r.shell(r.policy, fmt.Sprintf("kill -TERM %d", outside.Pid)); err == nil {
		t.Errorf("a signal to an outside process was allowed: %q", out)
	}
	select {
	case <-outside.Exited:
		t.Error("the outside process died")
	case <-time.After(300 * time.Millisecond):
	}
	out, err := r.shell(r.policy, "sleep 30 & child=$!; kill -TERM $child; wait $child; echo status $?")
	if err != nil || !strings.HasSuffix(out, "status 143") {
		t.Errorf("a signal to its own child: %q %v", out, err)
	}
}
