//go:build linux

package sandbox_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos/internal/sandbox"
)

func TestLandlockConfinesRun(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	ws, ref, scratch := filepath.Join(root, "ws"), filepath.Join(root, "ref"), filepath.Join(root, "scratch")
	for _, dir := range []string{ws, ref, scratch} {
		os.MkdirAll(dir, 0o755)
	}
	os.WriteFile(filepath.Join(root, "secret.txt"), []byte("hunter2"), 0o600)
	policy := sandbox.Policy{Read: []string{ref}, Write: []string{ws}, Temp: scratch}
	run := func(p sandbox.Policy, command string) (string, error) {
		cmd, err := sandbox.Command(context.Background(), p, ws, command)
		if err != nil {
			return "", err
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(policy, "echo ok > inside.txt && cat inside.txt"); err != nil || strings.TrimSpace(out) != "ok" {
		t.Fatalf("in scope: %q %v", out, err)
	}
	for _, command := range []string{"cat " + filepath.Join(root, "secret.txt"), "echo x > " + filepath.Join(root, "outside.txt"), "echo x > " + filepath.Join(ref, "verdict.txt")} {
		if out, err := run(policy, command); err == nil || strings.Contains(out, "hunter2") {
			t.Fatalf("%q was allowed: %q", command, out)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	// bash's /dev/tcp where there is bash, else busybox nc.
	probe := "if command -v bash >/dev/null; then bash -c 'exec 3<>/dev/tcp/127.0.0.1/" + itoa(port) + "'; else nc -z -w 2 127.0.0.1 " + itoa(port) + "; fi"
	if _, err := run(policy, probe); err == nil {
		t.Fatal("a TCP connection was allowed without network")
	}
	networked := policy
	networked.Network = true
	if out, err := run(networked, probe); err != nil {
		t.Fatalf("with network: %q %v", out, err)
	}
	nested := policy
	nested.Hidden = []string{filepath.Join(ws, ".aos-state")}
	if _, err := sandbox.Command(context.Background(), nested, ws, "true"); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("a hidden root inside a write root cannot be expressed and must be refused: %v", err)
	}
}

func itoa(n int) string {
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
