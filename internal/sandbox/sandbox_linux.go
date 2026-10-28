//go:build linux

package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"
)

// Landlock confines a process to the file trees it names (allow-list only)
// and, from ABI 4, TCP connect and bind. It is applied by a helper: the
// binary re-executes itself with helperArg, restricts itself, then execs
// the shell, which inherits the restriction.
//
// Limits: Landlock cannot deny a subtree inside a granted one, so a policy
// whose read-only or hidden root lies inside a read or write root is
// refused; it does not cover UDP, so without network a command can still
// send UDP (DNS lookups included); and before ABI 4 network cannot be
// confined at all, so a policy without network is refused there.

const (
	helperArg = "__aos_sandbox_exec"
	policyEnv = "AOS_SANDBOX_POLICY"

	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446
	createRulesetVersion     = 1
	rulePathBeneath          = 1
	prSetNoNewPrivs          = 38
	oPath                    = 0x200000 // O_PATH, missing from syscall on some arches

	accessExecute    = 1 << 0
	accessWriteFile  = 1 << 1
	accessReadFile   = 1 << 2
	accessReadDir    = 1 << 3
	accessRemoveDir  = 1 << 4
	accessRemoveFile = 1 << 5
	accessMakeChar   = 1 << 6
	accessMakeDir    = 1 << 7
	accessMakeReg    = 1 << 8
	accessMakeSock   = 1 << 9
	accessMakeFifo   = 1 << 10
	accessMakeBlock  = 1 << 11
	accessMakeSym    = 1 << 12
	accessRefer      = 1 << 13
	accessTruncate   = 1 << 14
	accessIoctlDev   = 1 << 15

	accessNetBindTCP    = 1 << 0
	accessNetConnectTCP = 1 << 1
)

var systemReadPaths = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/opt", "/proc", "/sys", "/dev", "/run"}

var devices = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/tty", "/dev/urandom", "/dev/random"}

func init() {
	if len(os.Args) > 1 && os.Args[1] == helperArg {
		os.Exit(confineAndExec(os.Args[2:]))
	}
}

// abi is the Landlock ABI version the kernel supports, 0 for none.
func abi() int {
	version, _, errno := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, createRulesetVersion)
	if errno != 0 {
		return 0
	}
	return int(version)
}

// Available reports whether commands can be confined here.
func Available() error {
	if abi() < 1 {
		return fmt.Errorf("%w: Landlock is not enabled in this kernel", ErrUnavailable)
	}
	return nil
}

// Command returns `sh -c command` confined to p, run from dir, through the
// re-exec helper.
func Command(ctx context.Context, p Policy, dir, command string) (*exec.Cmd, error) {
	if err := check(p); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, self, helperArg, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(p.Environment(os.Environ()), policyEnv+"="+string(encoded))
	return cmd, nil
}

// check refuses what Landlock cannot express here.
func check(p Policy) error {
	version := abi()
	if version < 1 {
		return fmt.Errorf("%w: Landlock is not enabled in this kernel", ErrUnavailable)
	}
	if !p.Network && version < 4 {
		return fmt.Errorf("%w: Landlock ABI %d cannot deny network (needs 4)", ErrUnavailable, version)
	}
	if p.Temp == "" {
		return fmt.Errorf("%w: no scratch directory", ErrUnavailable)
	}
	granted := append(append(append([]string{}, p.Read...), p.Write...), systemReadPaths...)
	granted = append(granted, ToolchainReadPaths()...)
	for _, denied := range append(append([]string{}, p.ReadOnly...), p.Hidden...) {
		for _, root := range granted {
			if within(root, denied) && root != denied {
				return fmt.Errorf("%w: %s lies inside granted %s, which Landlock cannot carve out", ErrUnavailable, denied, root)
			}
		}
	}
	for _, readOnly := range p.ReadOnly {
		for _, root := range p.Write {
			if within(root, readOnly) {
				return fmt.Errorf("%w: read-only %s lies inside writable %s", ErrUnavailable, readOnly, root)
			}
		}
	}
	return nil
}

// confineAndExec runs in the helper: restrict this thread, then exec the
// command on it, so the command starts confined.
func confineAndExec(args []string) int {
	runtime.LockOSThread()
	var p Policy
	if err := json.Unmarshal([]byte(os.Getenv(policyEnv)), &p); err != nil || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "aos sandbox: bad helper invocation")
		return 126
	}
	if err := restrict(p); err != nil {
		fmt.Fprintln(os.Stderr, "aos sandbox:", err)
		return 126
	}
	var env []string
	for _, entry := range os.Environ() {
		if len(entry) < len(policyEnv)+1 || entry[:len(policyEnv)+1] != policyEnv+"=" {
			env = append(env, entry)
		}
	}
	err := syscall.Exec(args[0], args, env)
	fmt.Fprintln(os.Stderr, "aos sandbox: exec:", err)
	return 126
}

func restrict(p Policy) error {
	version := abi()
	if version < 1 {
		return ErrUnavailable
	}
	handled := uint64(1<<13 - 1)
	if version >= 2 {
		handled |= accessRefer
	}
	if version >= 3 {
		handled |= accessTruncate
	}
	if version >= 5 {
		handled |= accessIoctlDev
	}
	readRights := uint64(accessExecute | accessReadFile | accessReadDir)
	attr := make([]byte, 16)
	binary.LittleEndian.PutUint64(attr[0:], handled)
	size := 8
	if version >= 4 {
		size = 16
		if !p.Network {
			binary.LittleEndian.PutUint64(attr[8:], accessNetBindTCP|accessNetConnectTCP)
		}
	}
	fd, _, errno := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr[0])), uintptr(size), 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	defer syscall.Close(int(fd))
	add := func(path string, rights uint64) error {
		file, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil // a missing path grants nothing
		}
		defer syscall.Close(file)
		var stat syscall.Stat_t
		if syscall.Fstat(file, &stat) == nil && stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			rights &= accessExecute | accessWriteFile | accessReadFile | accessTruncate | accessIoctlDev
		}
		rule := make([]byte, 12)
		binary.LittleEndian.PutUint64(rule[0:], rights&handled)
		binary.LittleEndian.PutUint32(rule[8:], uint32(file))
		if _, _, errno := syscall.Syscall6(sysLandlockAddRule, fd, rulePathBeneath, uintptr(unsafe.Pointer(&rule[0])), 0, 0, 0); errno != 0 {
			return fmt.Errorf("landlock_add_rule %s: %w", path, errno)
		}
		return nil
	}
	for _, path := range append(append(append([]string{}, systemReadPaths...), ToolchainReadPaths()...), p.Read...) {
		if err := add(path, readRights); err != nil {
			return err
		}
	}
	for _, path := range devices {
		if err := add(path, readRights|accessWriteFile|accessTruncate|accessIoctlDev); err != nil {
			return err
		}
	}
	for _, path := range append([]string{p.Temp}, p.Write...) {
		if err := add(path, handled); err != nil {
			return err
		}
	}
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(NO_NEW_PRIVS): %w", errno)
	}
	if _, _, errno := syscall.Syscall(sysLandlockRestrictSelf, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}
