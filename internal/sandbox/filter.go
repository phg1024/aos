package sandbox

import "fmt"

// The socket filter closes what Landlock leaves open: it has no control over
// connecting to a Unix socket by path before ABI 9, so a command could reach the
// user's service manager and have it run anything outside the sandbox; and it does
// not cover UDP. The filter works on the syscall that makes a socket, since seccomp
// can read a socket's family and type but not a path.
//
// It returns EPERM rather than killing, so a tool fails with a readable error.
//
//   - socket() gets IPv4 and IPv6 only with network. Landlock answers for TCP.
//   - socket(AF_UNIX, SOCK_SEQPACKET) is allowed only with a device grant. CUDA
//     makes these and will not start without them. Such a socket cannot connect to a
//     stream or datagram listener, which is what the service manager and the buses
//     are. It can reach a seqpacket listener at a path the user may write to.
//   - socketpair() gets connected Unix pairs: stream and seqpacket. Python and build
//     tools use them, and they reach nothing outside the process tree. A datagram
//     pair is refused, since it could be pointed elsewhere.
//   - io_uring makes and connects sockets without calling socket().
//   - Another architecture's syscall numbers, which include x86-64's 32-bit entry
//     and x32's, would get past all of this.
const (
	afUnix  = 1
	afInet  = 2
	afInet6 = 10

	sockStream    = 1
	sockDgram     = 2
	sockSeqpacket = 5
	sockTypeMask  = 0xf

	sysIoUringSetup    = 425
	sysIoUringEnter    = 426
	sysIoUringRegister = 427
	x32SyscallBit      = 0x40000000

	seccompRetAllow = 0x7fff0000
	seccompRetEPERM = 0x00050000 | 1 // SECCOMP_RET_ERRNO | EPERM

	bpfLoadWord = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJumpEq   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJumpGe   = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	bpfAnd      = 0x54 // BPF_ALU | BPF_AND | BPF_K
	bpfReturn   = 0x06 // BPF_RET | BPF_K

	// Offsets into struct seccomp_data. The low word of an argument is what the
	// kernel reads for socket's int arguments, on both architectures here.
	dataNumber = 0
	dataArch   = 4
	dataArg0   = 16
	dataArg1   = 24
)

// filterArch is what the filter must know about the architecture it runs on.
type filterArch struct {
	audit              uint32
	socket, socketpair uint32
}

var filterArches = map[string]filterArch{
	"amd64": {audit: 0xc000003e, socket: 41, socketpair: 53},
	"arm64": {audit: 0xc00000b7, socket: 198, socketpair: 199},
}

// bpfInstruction is struct sock_filter.
type bpfInstruction struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

// socketFilter is the program for p on goarch. An architecture it does not know is
// ErrUnavailable, so the run is refused rather than left unfiltered.
func socketFilter(goarch string, p Policy) ([]bpfInstruction, error) {
	arch, ok := filterArches[goarch]
	if !ok {
		return nil, fmt.Errorf("%w: no socket filter for %s", ErrUnavailable, goarch)
	}
	a := newBPF()
	a.load(dataArch)
	a.jump(bpfJumpEq, arch.audit, "", "deny")
	a.load(dataNumber)
	a.jump(bpfJumpGe, x32SyscallBit, "deny", "")
	a.jump(bpfJumpEq, arch.socket, "socket", "")
	a.jump(bpfJumpEq, arch.socketpair, "socketpair", "")
	a.jump(bpfJumpEq, sysIoUringSetup, "deny", "")
	a.jump(bpfJumpEq, sysIoUringEnter, "deny", "")
	a.jump(bpfJumpEq, sysIoUringRegister, "deny", "allow")

	a.label("socket")
	a.load(dataArg0)
	if p.Network {
		a.jump(bpfJumpEq, afInet, "allow", "")
		a.jump(bpfJumpEq, afInet6, "allow", "")
	}
	if len(p.Devices) > 0 {
		a.jump(bpfJumpEq, afUnix, "", "deny")
		a.load(dataArg1)
		a.and(sockTypeMask)
		a.jump(bpfJumpEq, sockSeqpacket, "allow", "deny")
	} else {
		a.ret(seccompRetEPERM)
	}

	a.label("socketpair")
	a.load(dataArg0)
	a.jump(bpfJumpEq, afUnix, "", "deny")
	a.load(dataArg1)
	a.and(sockTypeMask)
	a.jump(bpfJumpEq, sockStream, "allow", "")
	a.jump(bpfJumpEq, sockSeqpacket, "allow", "deny")

	a.label("allow")
	a.ret(seccompRetAllow)
	a.label("deny")
	a.ret(seccompRetEPERM)
	return a.assemble(), nil
}

// bpfAssembler resolves jumps by name, since a wrong count silently allows.
type bpfAssembler struct {
	code   []bpfInstruction
	labels map[string]int
	jumps  []bpfJump
}

type bpfJump struct {
	at          int
	taken, fall string
}

func newBPF() *bpfAssembler { return &bpfAssembler{labels: map[string]int{}} }

func (a *bpfAssembler) load(offset uint32) {
	a.code = append(a.code, bpfInstruction{code: bpfLoadWord, k: offset})
}

func (a *bpfAssembler) and(mask uint32) {
	a.code = append(a.code, bpfInstruction{code: bpfAnd, k: mask})
}

func (a *bpfAssembler) ret(value uint32) {
	a.code = append(a.code, bpfInstruction{code: bpfReturn, k: value})
}

func (a *bpfAssembler) label(name string) { a.labels[name] = len(a.code) }

// jump goes to taken when the comparison holds and to fall otherwise. An empty name
// is the next instruction.
func (a *bpfAssembler) jump(code uint16, k uint32, taken, fall string) {
	a.jumps = append(a.jumps, bpfJump{len(a.code), taken, fall})
	a.code = append(a.code, bpfInstruction{code: code, k: k})
}

func (a *bpfAssembler) assemble() []bpfInstruction {
	for _, j := range a.jumps {
		if j.taken != "" {
			a.code[j.at].jt = uint8(a.labels[j.taken] - j.at - 1)
		}
		if j.fall != "" {
			a.code[j.at].jf = uint8(a.labels[j.fall] - j.at - 1)
		}
	}
	return a.code
}
