 #!/usr/bin/env bash
 set -euo pipefail

 NAME=mydocker
 MEM=100M
 CPU="50000 100000"
 PIDS=50
 APP=/Users/rvetoshkin/containerization-and-orchestration/lab-1-docker/api/api
 PROFILE=/Users/rvetoshkin/containerization-and-orchestration/lab-1-docker/seccomp/profile.json
 CG=/sys/fs/cgroup/$NAME

 echo "Шаг 1: скрипт запустился"

 sudo mkdir -p "$CG"
 echo "$MEM"  | sudo tee "$CG/memory.max" >/dev/null
 echo "$CPU"  | sudo tee "$CG/cpu.max"    >/dev/null
 echo "$PIDS" | sudo tee "$CG/pids.max"   >/dev/null

 echo "Шаг 2: cgroup создана, лимиты:"
 cat "$CG/memory.max" "$CG/cpu.max" "$CG/pids.max"

 ORIG_CG=/sys/fs/cgroup$(sed 's/^0:://' /proc/$$/cgroup)
 cleanup() {
   echo $$ | sudo tee "$ORIG_CG/cgroup.procs" >/dev/null
   sudo rmdir "$CG" && echo "cleanup: cgroup $CG удалена"
   }
 trap cleanup EXIT
 echo $$ | sudo tee "$CG/cgroup.procs" >/dev/null

 echo "Шаг 3: скрипт (PID $$) теперь в cgroup:"
 cat /proc/$$/cgroup
 cat "$CG/cgroup.procs"

 SECCOMP_RUN=$HOME/bin/seccomp-run
 DENY=$(jq -r '[.syscalls[] | select(.action=="SCMP_ACT_ERRNO") | .names[]] | join(",")' "$PROFILE")

 export APP SECCOMP_RUN DENY

 unshare --pid --mount --net --uts --ipc --user --fork --mount-proc --map-root-user \
 bash -c '
 ip link set lo up
 hostname mydocker
 exec setpriv --bounding-set -all --inh-caps -all --ambient-caps -all --no-new-privs  "$SECCOMP_RUN" "$DENY" -- "$APP"
 '
