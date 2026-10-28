//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const sandboxExec = "/usr/bin/sandbox-exec"

// systemReadPaths are readable by every confined command: the OS, the
// developer tools and the usual package prefixes. The home directory is
// not among them.
var systemReadPaths = []string{
	"/usr", "/bin", "/sbin", "/System", "/Library",
	"/private/etc", "/private/var/db/timezone", "/private/var/db/xcode_select_link",
	"/opt/homebrew", "/opt/local", "/Applications/Xcode.app", "/dev",
}

// devices are writable by every confined command.
var devices = []string{"/dev/null", "/dev/zero", "/dev/tty", "/dev/dtracehelper", "/dev/stdout", "/dev/stderr"}

// machServices are the only system services a confined command may reach:
// directory lookups, notifications and logging, and the Metal compiler and
// GPU services, which GPU work needs. Services that can reach the network
// (name resolution, URL sessions) are left out unless the policy allows
// network.
var machServices = []string{
	"com.apple.system.opendirectoryd.libinfo",
	"com.apple.system.opendirectoryd.membership",
	"com.apple.system.notification_center",
	"com.apple.system.logger",
	"com.apple.logd",
	"com.apple.diagnosticd",
	"com.apple.MTLCompilerService",
	"com.apple.gpumemd.source",
	"com.apple.MTLAssetUpgraderD",
	"com.apple.cvmsServ",
}

var networkServices = []string{
	"com.apple.dnssd.service",
	"com.apple.mDNSResponder",
	"com.apple.trustd",
	"com.apple.trustd.agent",
	"com.apple.SystemConfiguration.configd",
	"com.apple.networkd",
}

// Available reports whether commands can be confined here.
func Available() error {
	if _, err := os.Stat(sandboxExec); err != nil {
		return fmt.Errorf("%w: %s not found", ErrUnavailable, sandboxExec)
	}
	return nil
}

// Command returns `sh -c command` confined to p, run from dir.
func Command(ctx context.Context, p Policy, dir, command string) (*exec.Cmd, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	if p.Temp == "" {
		return nil, fmt.Errorf("%w: no scratch directory", ErrUnavailable)
	}
	cmd := exec.CommandContext(ctx, sandboxExec, "-p", Profile(p), "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = p.Environment(os.Environ())
	return cmd, nil
}

// Profile is the sandbox-exec profile for p. Later rules win, so the
// read-only and hidden roots, written last, override the grants above
// them.
func Profile(p Policy) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-fork)\n(allow process-exec)\n(allow signal (target same-sandbox))\n")
	b.WriteString("(allow sysctl-read)\n(allow iokit-open)\n(allow ipc-posix-shm)\n(allow ipc-posix-sem)\n")
	// Metadata (stat) anywhere: path resolution and getcwd need it. It
	// reveals that a path exists, not its contents.
	b.WriteString("(allow file-read-metadata)\n")
	services := machServices
	if p.Network {
		services = append(append([]string(nil), machServices...), networkServices...)
	}
	b.WriteString("(allow mach-lookup")
	for _, service := range services {
		fmt.Fprintf(&b, " (global-name %s) (xpc-service-name %s)", quote(service), quote(service))
	}
	b.WriteString(")\n")
	writeRule(&b, "allow file-read*", append([]string{}, systemReadPaths...), `(literal "/")`)
	writeRule(&b, "allow file-read*", ToolchainReadPaths(), "")
	b.WriteString("(allow file-read* file-write*")
	for _, device := range devices {
		fmt.Fprintf(&b, " (literal %s)", quote(device))
	}
	b.WriteString(")\n(allow file-ioctl (literal \"/dev/tty\"))\n")
	writeRule(&b, "allow file-read*", p.Read, "")
	writeRule(&b, "allow file-read* file-write*", append([]string{p.Temp}, p.Write...), "")
	writeRule(&b, "deny file-write*", p.ReadOnly, "")
	writeRule(&b, "deny file-read* file-write*", p.Hidden, "")
	if p.Network {
		b.WriteString("(allow network*)\n(allow system-socket)\n")
	}
	return b.String()
}

func writeRule(b *strings.Builder, rule string, paths []string, extra string) {
	if len(paths) == 0 && extra == "" {
		return
	}
	b.WriteString("(" + rule)
	if extra != "" {
		b.WriteString(" " + extra)
	}
	for _, path := range paths {
		fmt.Fprintf(b, " (subpath %s)", quote(path))
	}
	b.WriteString(")\n")
}

// quote writes s as a profile string literal.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
