#!/bin/bash
# Run inside the confinement. What a real workload does, one line a check.
# $1 is the writable directory, $2 is gpu when the policy grants the card.
w=$1; gpu=${2:-}
cd "$w" || exit 1
t() { local name=$1; shift; if out=$("$@" 2>&1); then printf '%-34s ok\n' "$name"; else printf '%-34s FAIL %s: %s\n' "$name" "$?" "$(printf '%s' "$out" | tail -1 | cut -c1-90)"; fi; }
py() { python3 -I -c "$1"; }
t "python multiprocessing Pool" py 'import multiprocessing as m; p=m.Pool(2); assert p.map(abs,[-1,-2])==[1,2]; p.close(); p.join()'
t "python multiprocessing Queue" py 'import multiprocessing as m; q=m.Queue(); q.put(1); assert q.get(timeout=5)==1'
t "python Pipe(duplex)" py 'import multiprocessing as m; a,b=m.Pipe(); a.send(1); assert b.recv()==1'
t "python forkserver" py 'import multiprocessing as m
if __name__=="__main__":
    p=m.get_context("forkserver").Pool(1); assert p.apply(abs,(-3,))==3; p.close(); p.join()'
t "python Manager" py 'import multiprocessing as m
if __name__=="__main__":
    d=m.Manager().dict(); d[1]=2; assert d[1]==2'
t "python asyncio subprocess" py 'import asyncio
async def f():
    p=await asyncio.create_subprocess_exec("true"); assert await p.wait()==0
asyncio.run(f())'
t "python getaddrinfo localhost" py 'import socket; socket.getaddrinfo("localhost",80)'
t "node child_process" node -e 'console.log(require("child_process").execSync("true").length)'
t "getent passwd" getent passwd "$(id -un)"
t "whoami" whoami
t "git init and commit" sh -c 'rm -rf g; git init -q g && cd g && git -c user.name=a -c user.email=a@a commit -q --allow-empty -m x && git log --oneline >/dev/null'
t "gcc -pipe and make" sh -c 'printf "int main(){return 0;}\n" > h.c; printf "h: h.c\n\tgcc -pipe -o h h.c\n" > Makefile; rm -f h; make -s && ./h'
t "cmake configure" sh -c 'rm -rf c; mkdir c; printf "cmake_minimum_required(VERSION 3.10)\nproject(x C)\n" > c/CMakeLists.txt; cmake -S c -B c/b >/dev/null'
if [[ $gpu == gpu ]]; then
  t "nvidia-smi -L" nvidia-smi -L
  t "cuInit, cuDeviceGetCount" py 'import ctypes
l=ctypes.CDLL("libcuda.so.1"); r=l.cuInit(0); assert r==0, "cuInit %d"%r
n=ctypes.c_int(); r=l.cuDeviceGetCount(ctypes.byref(n)); assert r==0 and n.value==1, "count %d %d"%(r,n.value)'
  t "nvcc compile, no run" sh -c 'printf "#include <cuda_runtime.h>\n__global__ void k(){}\nint main(){return 0;}\n" > k.cu; nvcc -std=c++17 k.cu -o k'
fi
