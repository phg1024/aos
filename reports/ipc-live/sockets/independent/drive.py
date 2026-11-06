import json, os, socket, subprocess, sys, time
root = sys.argv[1]; aos = root + "/bin/aos"; out = root + "/outside"
for d in ("bin","w","t","outside"): os.makedirs(root + "/" + d, exist_ok=True)
open(out + "/llm-key", "w").write("dummy-not-a-key\n"); os.chmod(out + "/llm-key", 0o600)
holder = subprocess.Popen(["/usr/bin/python3", "-I", root + "/bin/holder.py", root], env={"AOS_KEY_HOLDER": "dummy-holder-env", "PATH": "/usr/bin:/bin"})
time.sleep(1)
hpid, hfd = open(out + "/holder").read().split()
def count(): return open(out + "/count").read()
client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); client.connect(out + "/stream.sock")
fd = client.fileno(); os.set_inheritable(fd, True)
uuid = subprocess.run(["nvidia-smi","--query-gpu=uuid,name","--format=csv,noheader"],capture_output=True,text=True).stdout
uuid = [l for l in uuid.splitlines() if "3080" in l][0].split(",")[0]
minor = None
for g in os.listdir("/proc/driver/nvidia/gpus"):
    t = open("/proc/driver/nvidia/gpus/%s/information" % g).read()
    if uuid in t: minor = [l for l in t.splitlines() if "Device Minor" in l][0].split()[-1]
base = {"Read":[root+"/bin"],"Write":[root+"/w"],"Temp":root+"/t","Network":False}
env0 = {"PATH":"/usr/bin:/bin","HOME":os.environ["HOME"],"XDG_RUNTIME_DIR":"/run/user/%d"%os.getuid()}
probe = ["/usr/bin/python3","-I",root+"/bin/probe.py",root,hpid,hfd]
def clean():
    for p in ("/tmp/aos-book-review.fifo","/dev/shm/aos-book-review.probe",os.path.expanduser("~/.config/systemd/user/aos-book-review.probe")):
        try: os.unlink(p)
        except OSError: pass
def run(label, policy):
    print("\n########", label, "(count before", count() + ")"); sys.stdout.flush()
    if policy is None: cmd, env = probe, dict(env0)
    else: cmd, env = [aos,"__aos_sandbox_exec"]+probe, dict(env0, AOS_SANDBOX_POLICY=json.dumps(policy))
    r = subprocess.run(cmd, env=env, pass_fds=(fd,), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=120)
    print(r.stdout); print("exit", r.returncode); time.sleep(0.5); print("holder count after:", count()); sys.stdout.flush()
run("control: no sandbox", None)
clean()
c0 = count()
run("plain: no network, no devices", base)
run("network granted", dict(base, Network=True))
run("gpu: 3080 granted, no network", dict(base, Devices=["/dev/nvidiactl","/dev/nvidia-uvm","/dev/nvidia"+minor]))
print("\nholder count: after control", c0, "; after all confined", count())
holder.send_signal(10); time.sleep(0.3); print("holder count after one outside SIGUSR1:", count())
holder.kill()
print("left behind:", [p for p in ("/tmp/aos-book-review.fifo","/dev/shm/aos-book-review.probe",os.path.expanduser("~/.config/systemd/user/aos-book-review.probe")) if os.path.exists(p)])
print("card:", subprocess.run(["nvidia-smi","--query-compute-apps=pid,used_memory","--format=csv,noheader"],capture_output=True,text=True).stdout.replace("\n"," | "))
