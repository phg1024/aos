#!/bin/bash
# Runs on the machine with the card. Nothing here starts a unit, a job or a kernel,
# and systemctl below only asks. Usage: run.sh <root>
set -u
root=$1; b=$root/bin; w=$root/w; tmp=$root/t; own=$root/outside/sock; abstract=aos-sockprobe-$$
uid=$(id -u); rt=/run/user/$uid
mkdir -p "$w" "$tmp" "$root/outside"
env_base=(PATH="$HOME/aos-cuda/env/bin:/usr/bin:/bin" HOME="$HOME" XDG_RUNTIME_DIR="$rt" TMPDIR="$tmp")
uuid=$(nvidia-smi --query-gpu=uuid --format=csv,noheader | head -1)
minor=$(grep -l "$uuid" /proc/driver/nvidia/gpus/*/information | xargs grep 'Device Minor' | awk '{print $NF}')

policy() { # gpu|plain net|nonet
  local dev='' net=false
  [[ $1 == gpu ]] && dev='"/dev/nvidiactl","/dev/nvidia-uvm","/dev/nvidia'$minor'"'
  [[ $2 == net ]] && net=true
  printf '{"Read":["%s","%s"],"Write":["%s"],"Network":%s,"Devices":[%s],"Temp":"%s"}' "$b" "$HOME/aos-cuda/env" "$w" "$net" "$dev" "$tmp"
}
helper=("$b/aos" __aos_sandbox_exec)
bw_hide=(bwrap --ro-bind / / --dev-bind /dev /dev --proc /proc --tmpfs /run --tmpfs /tmp --bind "$w" "$w" --bind "$tmp" "$tmp")
bw_allow=(bwrap --ro-bind /usr /usr --symlink usr/bin /bin --symlink usr/sbin /sbin --symlink usr/lib /lib --symlink usr/lib64 /lib64 --ro-bind /etc /etc --ro-bind /sys /sys
  --dev-bind /dev /dev --proc /proc --tmpfs /run --tmpfs /tmp --ro-bind "$b" "$b" --ro-bind "$HOME/aos-cuda/env" "$HOME/aos-cuda/env" --bind "$w" "$w" --bind "$tmp" "$tmp")

# outer = what wraps the helper; the shipped rules run inside it in every case.
one() { # label gpu|plain net|nonet outer... -- script
  local label=$1 g=$2 n=$3; shift 3
  local outer=(); while [[ $1 != -- ]]; do outer+=("$1"); shift; done; shift
  echo "=== $label ($g, $n)"
  env -i "${env_base[@]}" CUDA_VISIBLE_DEVICES="$uuid" AOS_SANDBOX_POLICY="$(policy "$g" "$n")" "${outer[@]}" "${helper[@]}" /bin/bash -c "$1" 2>&1
  echo "exit $?"
}
"$b/sockprobe" serve "$own" "$abstract" 500 & server=$!
sleep 1
P="$b/sockprobe probe $rt/bus $rt/systemd/private /run/dbus/system_bus_socket $own $abstract"
S="systemctl --user is-system-running 2>&1 | sed 's/^/systemctl --user: /'"
W="bash $b/workload.sh $w"

echo "kernel $(uname -r), io_uring_disabled=$(cat /proc/sys/kernel/io_uring_disabled)"
echo "=== namespace, as aos itself would make it"; "$b/sockprobe" userns 2>&1
echo "=== bwrap"; ls -l /usr/bin/bwrap | cut -c1-10; ls /etc/apparmor.d | grep -E '^bwrap' 
for v in ${VARIANTS:-shipped strict seq seq-noinet bwrap-hide bwrap-allow}; do
  case $v in
    shipped) o=();; strict|seq*) o=("$b/sockprobe" filter "$v");; bwrap-hide) o=("${bw_hide[@]}");; bwrap-allow) o=("${bw_allow[@]}");;
  esac
  one "$v: reach" plain nonet "${o[@]}" -- "$P; $S"
  one "$v: workload" gpu nonet "${o[@]}" -- "$W gpu"
done
if [[ -n ${VARIANTS:-} ]]; then kill $server 2>/dev/null; exit 0; fi
one "seq, network granted: reach" plain net "$b/sockprobe" filter seq -- "$P; $S"
one "seq, network granted: lookups" plain net "$b/sockprobe" filter seq -- "python3 -I -c 'import socket;print(len(socket.getaddrinfo(\"example.com\",443)))' 2>&1 | tail -1; getent hosts localhost | head -1"
one "shipped, network granted: lookups" plain net -- "python3 -I -c 'import socket;print(len(socket.getaddrinfo(\"example.com\",443)))' 2>&1 | tail -1; getent hosts localhost | head -1"
echo "=== what CUDA asks for, unconfined"
strace -f -qq -e trace=socket,socketpair,connect,bind,listen,accept,accept4 -o "$tmp/cuda.strace" /usr/bin/python3 -I -c 'import ctypes;print("cuInit",ctypes.CDLL("libcuda.so.1").cuInit(0))' 2>&1
grep -E 'socket|connect|bind|listen|accept' "$tmp/cuda.strace" | sed -E 's/^[0-9]+ +//' | sort | uniq -c | cut -c1-140
echo "=== what nvidia-smi asks for, unconfined"
strace -f -qq -e trace=socket,socketpair,connect -o "$tmp/smi.strace" nvidia-smi -L >/dev/null 2>&1
grep -E 'socket|connect' "$tmp/smi.strace" | sed -E 's/^[0-9]+ +//' | sort | uniq -c | cut -c1-140
kill $server 2>/dev/null
echo "=== on the card now"; nvidia-smi --query-compute-apps=pid,name,used_memory --format=csv,noheader | awk -F, '{print "compute app:",$2,$3}'
echo "=== failed user units"; systemctl --user --failed --no-legend --no-pager | awk '{print $1,$2}'
echo "=== seqpacket listeners on this machine"
ss -xlH 2>/dev/null | awk '$1=="u_seq"{print $5}' | sort -u | while read -r s; do [[ $s == "$own"* ]] && continue; if [[ $s == /* ]]; then printf '%s %s\n' "$(stat -c '%a %U' "$s" 2>/dev/null)" "$s"; else printf 'abstract %s\n' "$s"; fi; done
echo "=== listeners by type"; ss -xlH 2>/dev/null | awk '{print $1}' | sort | uniq -c
