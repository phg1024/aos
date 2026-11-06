import fcntl, json, os, signal, subprocess, sys, time
root = sys.argv[1]; aos = root + "/bin/aos"; out = root + "/outside"
# Holder holds the dummy key file open and counts SIGUSR1 and SIGIO separately.
holder_src = r'''
import os, signal, sys, time
out = sys.argv[1]
keep = open(out + "/llm-key")
n = [0]
def on(*_):
    n[0] += 1
    open(out + "/count", "w").write(str(n[0]))
signal.signal(signal.SIGUSR1, on)
signal.signal(signal.SIGIO, on)
open(out + "/count", "w").write("0")
open(out + "/holder", "w").write("%d %d\n" % (os.getpid(), keep.fileno()))
time.sleep(120)
'''
open(root + "/bin/h2.py", "w").write(holder_src)
probe_src = r'''
import errno, fcntl, os, sys
hpid, kfd, mode = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
def attempt(label, f):
    try: print("%-40s OPEN %s" % (label, str(f())[:40]))
    except OSError as x: print("%-40s %s" % (label, errno.errorcode.get(x.errno, x.errno)))
if mode == "sigio":
    rd, wr = os.pipe()
    fcntl.fcntl(rd, fcntl.F_SETOWN, hpid)
    fcntl.fcntl(rd, fcntl.F_SETFL, fcntl.fcntl(rd, fcntl.F_GETFL) | os.O_ASYNC)
    os.write(wr, b"x")
    print("sigio armed and written")
else:
    attempt("readlink /proc/holder/fd/N", lambda: os.readlink("/proc/%d/fd/%d" % (hpid, kfd)))
    attempt("open /proc/holder/fd/N", lambda: open("/proc/%d/fd/%d" % (hpid, kfd)).read())
'''
open(root + "/bin/p2.py", "w").write(probe_src)
h = subprocess.Popen(["/usr/bin/python3", "-I", root + "/bin/h2.py", out])
time.sleep(1)
hpid, kfd = open(out + "/holder").read().split()
def count(): return open(out + "/count").read()
policy = {"Read":[root+"/bin"],"Write":[root+"/w"],"Temp":root+"/t","Network":False}
def go(label, confined, mode):
    argv = ["/usr/bin/python3","-I",root+"/bin/p2.py",hpid,kfd,mode]
    env = {"PATH":"/usr/bin:/bin","HOME":os.environ["HOME"]}
    if confined:
        argv = [aos,"__aos_sandbox_exec"]+argv; env["AOS_SANDBOX_POLICY"]=json.dumps(policy)
    before = count()
    r = subprocess.run(argv, env=env, capture_output=True, text=True, timeout=30)
    time.sleep(0.7)
    print("##", label, "rc", r.returncode, "| count", before, "->", count()); print(r.stdout + r.stderr, end="")
go("SIGIO, no sandbox", False, "sigio")
go("SIGIO, confined", True, "sigio")
go("SIGIO, no sandbox again", False, "sigio")
go("/proc fd, no sandbox", False, "fd")
go("/proc fd, confined", True, "fd")
h.kill()
