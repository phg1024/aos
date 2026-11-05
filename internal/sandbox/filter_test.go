package sandbox

import (
	"encoding/binary"
	"errors"
	"testing"
)

// interpret runs a filter over one syscall, as the kernel would.
func interpret(t *testing.T, program []bpfInstruction, arch, number uint32, arg0, arg1 uint64) uint32 {
	t.Helper()
	data := make([]byte, 64)
	binary.LittleEndian.PutUint32(data[dataNumber:], number)
	binary.LittleEndian.PutUint32(data[dataArch:], arch)
	binary.LittleEndian.PutUint64(data[dataArg0:], arg0)
	binary.LittleEndian.PutUint64(data[dataArg1:], arg1)
	var accumulator uint32
	for pc := 0; pc < len(program); pc++ {
		i := program[pc]
		branch := func(holds bool) {
			if holds {
				pc += int(i.jt)
			} else {
				pc += int(i.jf)
			}
		}
		switch i.code {
		case bpfLoadWord:
			accumulator = binary.LittleEndian.Uint32(data[i.k:])
		case bpfAnd:
			accumulator &= i.k
		case bpfJumpEq:
			branch(accumulator == i.k)
		case bpfJumpGe:
			branch(accumulator >= i.k)
		case bpfReturn:
			return i.k
		default:
			t.Fatalf("instruction %#x at %d", i.code, pc)
		}
	}
	t.Fatal("the filter ran off its end")
	return 0
}

// What each policy gets, on each architecture.
func TestSocketFilterDecides(t *testing.T) {
	const afNetlink, afPacket, afVsock, sockRaw, sockCloexec = 16, 17, 40, 3, 0o2000000
	plain := Policy{}
	network := Policy{Network: true}
	gpu := Policy{Devices: []string{"/dev/nvidia0"}}
	both := Policy{Network: true, Devices: []string{"/dev/nvidia0"}}
	for goarch, arch := range filterArches {
		socket, pair := arch.socket, arch.socketpair
		cases := []struct {
			name         string
			number       uint32
			arg0, arg1   uint64
			plain        bool
			network, gpu bool
			both         bool
		}{
			{"stream Unix", socket, afUnix, sockStream, false, false, false, false},
			{"datagram Unix", socket, afUnix, sockDgram, false, false, false, false},
			{"seqpacket Unix", socket, afUnix, sockSeqpacket, false, false, true, true},
			{"seqpacket Unix, close on exec", socket, afUnix, sockSeqpacket | sockCloexec, false, false, true, true},
			{"stream Unix, close on exec", socket, afUnix, sockStream | sockCloexec, false, false, false, false},
			{"IPv4", socket, afInet, sockStream, false, true, false, true},
			{"IPv4 datagram", socket, afInet, sockDgram, false, true, false, true},
			{"IPv6", socket, afInet6, sockStream, false, true, false, true},
			{"netlink", socket, afNetlink, sockRaw, false, false, false, false},
			{"packet", socket, afPacket, sockRaw, false, false, false, false},
			{"vsock", socket, afVsock, sockStream, false, false, false, false},
			{"a family only in the high word", socket, 1 << 32, sockStream, false, false, false, false},
			{"stream pair", pair, afUnix, sockStream, true, true, true, true},
			{"seqpacket pair", pair, afUnix, sockSeqpacket, true, true, true, true},
			{"datagram pair", pair, afUnix, sockDgram, false, false, false, false},
			{"a pair of another family", pair, afInet, sockStream, false, false, false, false},
			{"io_uring_setup", sysIoUringSetup, 2, 0, false, false, false, false},
			{"io_uring_enter", sysIoUringEnter, 0, 0, false, false, false, false},
			{"io_uring_register", sysIoUringRegister, 0, 0, false, false, false, false},
			{"x32 socket", x32SyscallBit + 41, afInet, sockStream, false, false, false, false},
			{"an unrelated syscall", 1000, afUnix, sockStream, true, true, true, true},
		}
		for _, policy := range []struct {
			name   string
			policy Policy
			pick   func(int) bool
		}{
			{"plain", plain, func(i int) bool { return cases[i].plain }},
			{"network", network, func(i int) bool { return cases[i].network }},
			{"gpu", gpu, func(i int) bool { return cases[i].gpu }},
			{"network and gpu", both, func(i int) bool { return cases[i].both }},
		} {
			program, err := socketFilter(goarch, policy.policy)
			if err != nil {
				t.Fatal(err)
			}
			for i, c := range cases {
				want := uint32(seccompRetEPERM)
				if policy.pick(i) {
					want = seccompRetAllow
				}
				if got := interpret(t, program, arch.audit, c.number, c.arg0, c.arg1); got != want {
					t.Errorf("%s, %s: %s: %#x, want %#x", goarch, policy.name, c.name, got, want)
				}
			}
		}
	}
}

// Another architecture's numbers mean something else, and x86-64 takes 32-bit calls
// too: socket, socketcall and an ordinary read all stop.
func TestSocketFilterRefusesAForeignArchitecture(t *testing.T) {
	const i386, arm = 0x40000003, 0x40000028
	for goarch := range filterArches {
		program, err := socketFilter(goarch, Policy{Network: true, Devices: []string{"/dev/nvidia0"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, foreign := range []uint32{i386, arm} {
			for _, number := range []uint32{359, 102, 3} {
				if got := interpret(t, program, foreign, number, afInet, sockStream); got != seccompRetEPERM {
					t.Errorf("%s: arch %#x, syscall %d: %#x", goarch, foreign, number, got)
				}
			}
		}
	}
}

func TestSocketFilterRefusesAnUnknownArchitecture(t *testing.T) {
	if _, err := socketFilter("riscv64", Policy{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an architecture with no filter must refuse the run: %v", err)
	}
}
