//go:build linux && amd64

// sockprobe measures whether a confined command can reach a Unix socket, and two
// ways of stopping it. It changes nothing in aos: "filter" installs a seccomp filter
// and then execs the real aos helper, so the rules under test are the shipped ones.
//
//	sockprobe serve <path> <abstract> <seconds>  listen, outside any confinement
//	sockprobe filter <mode> <prog> [args...]     install the filter, exec prog
//	sockprobe userns                             try a namespace as aos itself would
//	sockprobe probe <bus> <private> <system> <own> <abstract>
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func int80(nr, a, b, c uintptr) uintptr

const (
	afUnix, afInet, afInet6, afNetlink, afPacket, afVsock = 1, 2, 10, 16, 17, 40
	sockStream, sockDgram, sockSeqpacket                  = 1, 2, 5

	archX8664                                   = 0xc000003e
	nrSocket, nrSocketpair                      = 41, 53
	nrUringSetup, nrUringEnter, nrUringRegister = 425, 426, 427
	x32Bit                                      = 0x40000000

	retAllow = 0x7fff0000
	retEPERM = 0x00050000 | 1

	bpfLdAbsW                        = 0x20
	bpfJeqK                          = 0x15
	bpfJgeK                          = 0x35
	bpfAndK                          = 0x54
	bpfRetK                          = 0x06
	offNr, offArch, offArg0, offArg1 = 0, 4, 16, 24
)

type insn struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

type jump struct {
	pc   int
	t, f string
}

type asm struct {
	code   []insn
	labels map[string]int
	jumps  []jump
}

func (a *asm) ld(off uint32)  { a.code = append(a.code, insn{bpfLdAbsW, 0, 0, off}) }
func (a *asm) and(k uint32)   { a.code = append(a.code, insn{bpfAndK, 0, 0, k}) }
func (a *asm) ret(v uint32)   { a.code = append(a.code, insn{bpfRetK, 0, 0, v}) }
func (a *asm) label(n string) { a.labels[n] = len(a.code) }
func (a *asm) j(c uint16, k uint32, t, f string) {
	a.jumps = append(a.jumps, jump{len(a.code), t, f})
	a.code = append(a.code, insn{c, 0, 0, k})
}
func (a *asm) done() []insn {
	for _, j := range a.jumps {
		if j.t != "" {
			a.code[j.pc].jt = uint8(a.labels[j.t] - j.pc - 1)
		}
		if j.f != "" {
			a.code[j.pc].jf = uint8(a.labels[j.f] - j.pc - 1)
		}
	}
	return a.code
}

// program is the filter under test, in three modes.
//
// strict: new sockets may only be IPv4 or IPv6, which Landlock answers for over TCP.
// seq: strict, and also socket(AF_UNIX, SOCK_SEQPACKET), which CUDA asks for. Such a
// socket cannot connect to a stream or datagram listener, which is what the user's
// bus and service manager are.
// seq-noinet: seq with no IPv4 or IPv6 either, for a policy without network, which
// closes UDP too.
// A -scope suffix also makes abstract Unix sockets outside this Landlock domain
// unreachable, which a seqpacket socket could otherwise get to.
//
// In every mode a socketpair may only be a connected Unix pair: it reaches nothing
// outside the process tree and cannot be pointed elsewhere. io_uring can make sockets
// without calling socket(), and another architecture's numbers would get past all of it.
func program(mode string) []insn {
	a := &asm{labels: map[string]int{}}
	a.ld(offArch)
	a.j(bpfJeqK, archX8664, "", "deny")
	a.ld(offNr)
	a.j(bpfJgeK, x32Bit, "deny", "")
	a.j(bpfJeqK, nrSocket, "socket", "")
	a.j(bpfJeqK, nrSocketpair, "pair", "")
	a.j(bpfJeqK, nrUringSetup, "deny", "")
	a.j(bpfJeqK, nrUringEnter, "deny", "")
	a.j(bpfJeqK, nrUringRegister, "deny", "allow")
	a.label("socket")
	a.ld(offArg0)
	inet := "allow"
	if strings.HasPrefix(mode, "seq-noinet") {
		inet = "deny"
	}
	a.j(bpfJeqK, afInet, inet, "")
	a.j(bpfJeqK, afInet6, inet, "")
	if mode == "strict" {
		a.j(bpfJeqK, afUnix, "deny", "deny")
	} else {
		a.j(bpfJeqK, afUnix, "", "deny")
		a.ld(offArg1)
		a.and(0xf)
		a.j(bpfJeqK, sockSeqpacket, "allow", "deny")
	}
	a.label("pair")
	a.ld(offArg0)
	a.j(bpfJeqK, afUnix, "", "deny")
	a.ld(offArg1)
	a.and(0xf)
	a.j(bpfJeqK, sockStream, "allow", "")
	a.j(bpfJeqK, sockSeqpacket, "allow", "deny")
	a.label("allow")
	a.ret(retAllow)
	a.label("deny")
	a.ret(retEPERM)
	return a.done()
}

func installFilter(mode string) error {
	prog := program(mode)
	fprog := struct {
		len    uint16
		filter *insn
	}{uint16(len(prog)), &prog[0]}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %w", e)
	}
	if strings.HasSuffix(mode, "-scope") {
		attr := [3]uint64{0, 0, 1} // handled fs, handled net, LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET
		fd, _, e := syscall.RawSyscall(444, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
		if e != 0 {
			return fmt.Errorf("landlock_create_ruleset: %w", e)
		}
		if _, _, e := syscall.RawSyscall(446, fd, 0, 0); e != 0 {
			return fmt.Errorf("landlock_restrict_self: %w", e)
		}
		syscall.Close(int(fd))
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, 22, 2, uintptr(unsafe.Pointer(&fprog))); e != 0 {
		return fmt.Errorf("seccomp: %w", e)
	}
	return nil
}

func show(name string, err error) {
	if err == nil {
		fmt.Printf("%-34s OPEN\n", name)
		return
	}
	fmt.Printf("%-34s %v\n", name, err)
}

func dial(path string) error {
	fd, err := syscall.Socket(afUnix, sockStream|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Connect(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

func dialSeq(path string) error {
	fd, err := syscall.Socket(afUnix, sockSeqpacket|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Connect(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

func raw(nr, a, b, c uintptr) error {
	fd, _, e := syscall.Syscall(nr, a, b, c)
	if e != 0 {
		return e
	}
	syscall.Close(int(fd))
	return nil
}

func pair(kind int, target string) {
	name := map[int]string{sockStream: "stream", sockDgram: "dgram", sockSeqpacket: "seqpacket"}[kind]
	fds, err := syscall.Socketpair(afUnix, kind|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		show("socketpair "+name, fmt.Errorf("socketpair: %w", err))
		return
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	show("socketpair "+name, nil)
	show("  repoint it with connect", syscall.Connect(fds[0], &syscall.SockaddrUnix{Name: target}))
	show("  repoint it with sendto", syscall.Sendto(fds[0], nil, 0, &syscall.SockaddrUnix{Name: target}))
}

func probe(bus, private, system, own, abstract string) {
	show("unix: user bus", dial(bus))
	show("unix: user systemd/private", dial(private))
	show("unix: system bus", dial(system))
	show("unix: own socket, outside grants", dial(own))
	show("unix: abstract", dial("@"+abstract))
	show("seqpacket: user bus", dialSeq(bus))
	show("seqpacket: user systemd/private", dialSeq(private))
	show("seqpacket: system bus", dialSeq(system))
	show("seqpacket: own stream socket", dialSeq(own))
	show("seqpacket: own seqpacket socket", dialSeq(own+".seq"))
	show("seqpacket: abstract seqpacket", dialSeq("@"+abstract+".seq"))
	pair(sockStream, bus)
	pair(sockSeqpacket, bus)
	pair(sockDgram, bus)
	show("socket(AF_NETLINK)", raw(nrSocket, afNetlink, syscall.SOCK_RAW, 0))
	show("socket(AF_VSOCK)", raw(nrSocket, afVsock, sockStream, 0))
	show("socket(AF_PACKET)", raw(nrSocket, afPacket, syscall.SOCK_RAW, 0))
	if fd, err := syscall.Socket(afInet, sockDgram, 0); err != nil {
		show("udp send to 127.0.0.1:9", fmt.Errorf("socket: %w", err))
	} else {
		show("udp send to 127.0.0.1:9", syscall.Sendto(fd, []byte("x"), 0, &syscall.SockaddrInet4{Port: 9, Addr: [4]byte{127, 0, 0, 1}}))
		syscall.Close(fd)
	}
	if fd, err := syscall.Socket(afInet, sockStream, 0); err != nil {
		show("tcp connect to 127.0.0.1:9", fmt.Errorf("socket: %w", err))
	} else {
		show("tcp connect to 127.0.0.1:9", syscall.Connect(fd, &syscall.SockaddrInet4{Port: 9, Addr: [4]byte{127, 0, 0, 1}}))
		syscall.Close(fd)
	}
	params := make([]byte, 120)
	fd, _, e := syscall.Syscall(nrUringSetup, 2, uintptr(unsafe.Pointer(&params[0])), 0)
	if e != 0 {
		show("io_uring_setup", e)
	} else {
		show("io_uring_setup", nil)
		syscall.Close(int(fd))
	}
	show("x32 socket(AF_UNIX)", raw(x32Bit+nrSocket, afUnix, sockStream, 0))
	if r := int64(int80(359, afUnix, sockStream, 0)); r < 0 {
		show("32-bit socket(AF_UNIX), int 0x80", syscall.Errno(-r))
	} else {
		show("32-bit socket(AF_UNIX), int 0x80", nil)
		syscall.Close(int(r))
	}
}

func serve(path, abstract string, seconds int) {
	os.Remove(path)
	os.Remove(path + ".seq")
	for _, l := range []struct{ net, addr string }{{"unix", path}, {"unix", "@" + abstract}, {"unixpacket", path + ".seq"}, {"unixpacket", "@" + abstract + ".seq"}} {
		l, err := net.Listen(l.net, l.addr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	}
	time.Sleep(time.Duration(seconds) * time.Second)
	os.Remove(path)
	os.Remove(path + ".seq")
}

// userns is what aos would have to do to hide sockets by itself: a Go process has
// threads, so a user namespace must be made at clone, not by unshare.
func userns() {
	uid, gid := os.Getuid(), os.Getgid()
	cmd := exec.Command(os.Args[0], "userns-child")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	show("clone(NEWUSER|NEWNS) with maps", cmd.Run())
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		var seconds int
		fmt.Sscan(os.Args[4], &seconds)
		serve(os.Args[2], os.Args[3], seconds)
	case "filter":
		runtime.LockOSThread()
		if err := installFilter(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "sockprobe:", err)
			os.Exit(126)
		}
		err := syscall.Exec(os.Args[3], os.Args[3:], os.Environ())
		fmt.Fprintln(os.Stderr, "sockprobe: exec:", err)
		os.Exit(126)
	case "userns":
		userns()
	case "userns-child":
		show("  mount tmpfs over /run/user", syscall.Mount("none", "/run/user", "tmpfs", 0, ""))
	case "probe":
		probe(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6])
	default:
		os.Exit(2)
	}
}
