#!/bin/bash

# Keep diagnostics best-effort; the failed test retains its original status.
as_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    else
        sudo "$@"
    fi
}

echo "Metadata service processes and memory"
ps -eo pid,ppid,rss,comm
free -h
for file in /sys/fs/cgroup/memory.events /sys/fs/cgroup/memory.peak; do
    if [ -r "$file" ]; then
        echo "$file"
        cat "$file"
    fi
done
as_root dmesg --ctime | tail -n 100

echo "TiUP launcher logs"
for file in tikv.log tidb.log; do
    if [ -f "$file" ]; then
        echo "$file"
        tail -n 200 "$file"
    fi
done

echo "TiUP service logs"
for directory in "$HOME/.tiup/data" /root/.tiup/data; do
    if as_root test -d "$directory"; then
        while IFS= read -r -d '' file; do
            echo "$file"
            as_root tail -n 100 "$file"
        done < <(as_root find "$directory" -type f -name '*.log' -print0)
    fi
done

exit 0
