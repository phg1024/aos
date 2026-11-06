import ctypes, os, signal, socket, sys, time
root = sys.argv[1]; out = root + "/outside"
libc = ctypes.CDLL(None, use_errno=True)
# Turn Yama off for this process alone, so any refusal a confined probe gets is the sandbox's.
libc.prctl(0x59616d61, ctypes.c_ulong(-1 & (2**64 - 1)), 0, 0, 0)
count = [0]
def on_usr1(*_):
    count[0] += 1
    open(out + "/count", "w").write(str(count[0]))
signal.signal(signal.SIGUSR1, on_usr1)
open(out + "/count", "w").write("0")
stream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); stream.bind(out + "/stream.sock"); stream.listen(8)
seq = socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET); seq.bind(out + "/seq.sock"); seq.listen(8)
aseq = socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET); aseq.bind("\0aos-book-review-seq"); aseq.listen(8)
astr = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); astr.bind("\0aos-book-review-str"); astr.listen(8)
a, b = socket.socketpair()
open(out + "/holder", "w").write("%d %d\n" % (os.getpid(), a.fileno()))
time.sleep(600)
