import ctypes, errno, fcntl, os, signal, socket, stat, subprocess, sys
root, hpid, hfd = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
out = root + "/outside"
libc = ctypes.CDLL(None, use_errno=True)
libc.syscall.restype = ctypes.c_long
def sc(*a):
    ctypes.set_errno(0)
    a = [ctypes.c_long(x) if isinstance(x, int) else x for x in a]
    r = libc.syscall(*a)
    return r, ctypes.get_errno()
def show(label, r, e):
    print("%-44s %s" % (label, "OPEN (%d)" % r if r >= 0 else errno.errorcode.get(e, str(e))))
    sys.stdout.flush()
    if r >= 0 and label.startswith("socket "):
        try: os.close(r)
        except OSError: pass
def attempt(label, f):
    try:
        v = f(); print("%-44s OPEN %s" % (label, "" if v is None else str(v)[:60]))
    except OSError as x:
        print("%-44s %s" % (label, errno.errorcode.get(x.errno, str(x.errno))))
    except Exception as x:
        print("%-44s %s" % (label, type(x).__name__))
    sys.stdout.flush()
for line in open("/proc/self/status"):
    if line.startswith(("NoNewPrivs", "Seccomp")): print(line.strip())
print("== inherited descriptors")
for fd in range(3, 64):
    try:
        if stat.S_ISSOCK(os.fstat(fd).st_mode): print("socket survives at fd", fd)
    except OSError: pass
print("== reaching the holder, a process of this user outside the sandbox")
r, e = sc(434, hpid, 0); show("pidfd_open", r, e)
if r >= 0:
    g, e3 = sc(438, r, hfd, 0); show("pidfd_getfd its socket", g, e3)
    s, e4 = sc(424, r, 0, 0, 0); show("pidfd_send_signal sig 0", s, e4)
    s, e4 = sc(424, r, 10, 0, 0); show("pidfd_send_signal SIGUSR1", s, e4)
r, e = sc(101, 0x4206, hpid, 0, 0); show("ptrace SEIZE", r, e)
if r >= 0: sc(101, 17, hpid, 0, 0)
buf = ctypes.create_string_buffer(8)
class IOV(ctypes.Structure): _fields_ = [("b", ctypes.c_void_p), ("n", ctypes.c_size_t)]
l = IOV(ctypes.addressof(buf), 8); rm = IOV(0x400000, 8)
r, e = sc(310, hpid, ctypes.byref(l), 1, ctypes.byref(rm), 1, 0); print("%-44s %s" % ("process_vm_readv", "OPEN" if r >= 0 else errno.errorcode.get(e, r)))
for name in ("environ", "mem", "maps"):
    attempt("/proc/holder/" + name, lambda: open("/proc/%d/%s" % (hpid, name), "rb").read(16))
attempt("/proc/holder/fd listing", lambda: len(os.listdir("/proc/%d/fd" % hpid)))
attempt("/proc/holder/root/etc/hostname", lambda: open("/proc/%d/root/etc/hostname" % hpid).read())
attempt("/proc/holder/cwd listing", lambda: len(os.listdir("/proc/%d/cwd" % hpid)))
print("== signals at it (the holder counts arrivals)")
attempt("kill SIGUSR1", lambda: os.kill(hpid, 10))
r, e = sc(234, hpid, hpid, 10); show("tgkill SIGUSR1", r, e)
si = ctypes.create_string_buffer(128); si[0:4] = (10).to_bytes(4, "little"); si[8:12] = (-1 & 0xffffffff).to_bytes(4, "little")
r, e = sc(129, hpid, 10, si); show("rt_sigqueueinfo SIGUSR1", r, e)
def sigio():
    rd, wr = os.pipe()
    fcntl.fcntl(rd, fcntl.F_SETSIG, 10)
    fcntl.fcntl(rd, fcntl.F_SETOWN, hpid)
    fl = fcntl.fcntl(rd, fcntl.F_GETFL); fcntl.fcntl(rd, fcntl.F_SETFL, fl | os.O_ASYNC)
    os.write(wr, b"x")
attempt("F_SETOWN then SIGIO write (see count)", sigio)
print("== own child")
c = subprocess.Popen(["/bin/sleep", "30"]); os.kill(c.pid, 15); print("child killed, status", c.wait())
print("== sockets, by family and type")
for f, t, n in ((1,1,"unix stream"),(1,2,"unix dgram"),(1,5,"unix seqpacket"),(1,5|0o2000000,"unix seqpacket|cloexec"),(1,1|0o2000000,"unix stream|cloexec"),(1,1|0o4000,"unix stream|nonblock"),(2,1,"inet stream"),(2,2,"inet dgram"),(10,2,"inet6 dgram"),(16,3,"netlink"),(17,3,"packet"),(40,1,"vsock"),(38,5,"alg"),(0x100000001,1,"high word, unix stream"),(0x100000002,1,"high word, inet stream")):
    r, e = sc(41, f, t, 0); show("socket " + n, r, e)
print("== reach, for whatever socket() allows")
def conn(t, addr):
    s = socket.socket(socket.AF_UNIX, t)
    try: s.connect(addr)
    finally: s.close()
rt = "/run/user/%d" % os.getuid()
for label, t, a in (("seqpacket -> user bus", 5, rt + "/bus"), ("seqpacket -> systemd/private", 5, rt + "/systemd/private"), ("seqpacket -> system bus", 5, "/run/dbus/system_bus_socket"), ("seqpacket -> outside stream", 5, out + "/stream.sock"), ("seqpacket -> outside seqpacket", 5, out + "/seq.sock"), ("seqpacket -> outside abstract seqpacket", 5, "\0aos-book-review-seq"), ("seqpacket -> outside abstract stream", 5, "\0aos-book-review-str"), ("stream -> user bus", 1, rt + "/bus")):
    attempt(label, lambda: conn(t, a))
print("== socketpair")
for t, n in ((1,"stream"),(2,"dgram"),(5,"seqpacket")):
    v = (ctypes.c_int * 2)()
    r, e = sc(53, 1, t, 0, v); print("%-44s %s" % ("socketpair unix " + n, "OPEN" if r == 0 else errno.errorcode[e]))
    if r == 0:
        s = socket.socket(fileno=v[0])
        attempt("  repoint at bus by connect", lambda: s.connect(rt + "/bus"))
        s.close(); os.close(v[1])
v = (ctypes.c_int * 2)(); r, e = sc(53, 2, 1, 0, v); print("%-44s %s" % ("socketpair inet", "OPEN" if r == 0 else errno.errorcode[e]))
print("== others")
p = ctypes.create_string_buffer(120)
r, e = sc(425, 4, p); show("io_uring_setup", r, e)
r, e = sc(426, 0, 0, 0, 0, 0, 0); print("%-44s %s" % ("io_uring_enter", errno.errorcode.get(e, e)))
pid = os.fork()
if pid == 0:
    r = libc.unshare(0x10000000 | 0x00020000 | 0x40000000)
    print("%-44s %s" % ("unshare user+mnt+net", "OPEN" if r == 0 else errno.errorcode[ctypes.get_errno()]))
    r2, e2 = sc(41, 1, 1, 0); print("%-44s %s" % ("  socket afterwards", "OPEN" if r2 >= 0 else errno.errorcode[e2]))
    m = libc.mount(b"none", root.encode() + b"/w", b"tmpfs", 0, None)
    print("%-44s %s" % ("  mount tmpfs afterwards", "OPEN" if m == 0 else errno.errorcode[ctypes.get_errno()]))
    if m == 0: libc.umount2(root.encode() + b"/w", 2)
    sys.stdout.flush(); os._exit(0)
os.waitpid(pid, 0)
attempt("read outside dummy key file", lambda: open(out + "/llm-key").read())
attempt("create in ~/.config/systemd/user", lambda: open(os.path.expanduser("~/.config/systemd/user/aos-book-review.probe"), "w").close())
attempt("list home", lambda: len(os.listdir(os.path.expanduser("~"))))
attempt("list ~/.ssh", lambda: len(os.listdir(os.path.expanduser("~/.ssh"))))
attempt("create in /dev/shm", lambda: open("/dev/shm/aos-book-review.probe", "w").close())
attempt("mkfifo in /tmp", lambda: os.mkfifo("/tmp/aos-book-review.fifo"))
attempt("read /proc/keys", lambda: len(open("/proc/keys").read().splitlines()))
r = subprocess.run(["/usr/bin/systemctl", "--user", "is-system-running"], capture_output=True, text=True); print("systemctl --user is-system-running: rc", r.returncode, (r.stdout + r.stderr).strip()[:110])
