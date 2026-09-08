#!/bin/sh
# Turn one opaque EPERM into a sentence naming what is missing.
#
# gpu-cuda-profile carries file capabilities, which is what lets it run as a
# non-root uid. The kernel refuses to exec a file whose file capabilities are
# not a subset of the process's BOUNDING set -- and it refuses with a bare
# EPERM, before any of the program's own code runs. So a container started
# without the capabilities does not report a missing capability: it reports
#
#   exec container process `/usr/local/bin/gpu-cuda-profile`: Operation not permitted
#
# which names neither the capability nor the securityContext that should have
# granted it, and in Kubernetes surfaces as a CrashLoopBackOff with nothing in
# it. This script checks the bounding set FIRST, so the common misconfiguration
# explains itself instead.
set -eu

BIN=/usr/local/bin/gpu-cuda-profile

# CapBnd is the bounding set as a hex mask in /proc/self/status. Bit N is
# capability N; the numbers below are from <linux/capability.h> and are spelled
# out because a wrong one here would report a capability the agent does not
# actually need.
bnd=$(awk '/^CapBnd:/ {print $2}' /proc/self/status)
missing=""
check() { # name bit
    if [ $(( (0x${bnd} >> $2) & 1 )) -eq 0 ]; then
        missing="${missing} $1"
    fi
}
check CAP_BPF 39                # load programs, create maps and links
check CAP_PERFMON 38            # perf_event_open
check CAP_SYS_PTRACE 19         # read another process's memory
check CAP_CHECKPOINT_RESTORE 40 # /proc/<pid>/map_files

if [ -n "${missing}" ]; then
    cat >&2 <<EOF
perf-agent: refusing to start -- these capabilities are not in this container's
bounding set:${missing}

The binary carries them as FILE capabilities so it can run as a non-root uid,
and the kernel will not exec such a file unless the same capabilities are in
the bounding set. Without this check the failure is a bare "Operation not
permitted" at exec, naming nothing.

In Kubernetes, grant them in the container's securityContext:

  securityContext:
    capabilities:
      drop: ["ALL"]
      add: ["BPF", "PERFMON", "SYS_PTRACE", "CHECKPOINT_RESTORE"]

With docker or podman: --cap-add=BPF,PERFMON,SYS_PTRACE,CHECKPOINT_RESTORE

CAP_SYS_ADMIN is deliberately NOT required and must not be added to work
around this -- it is near-root and is what gets a per-pod agent rejected by
admission policy.
EOF
    exit 1
fi

# CAP_SYSLOG is not in this binary's file capabilities and cannot be gained by
# granting it -- see Dockerfile.agent for why five would make the documented
# four-capability deployment fail to exec. Kernel frames are therefore not
# symbolized in this image. Said once, at start, because the profile it
# produces looks perfectly healthy without them.
if [ $(( (0x${bnd} >> 34) & 1 )) -eq 1 ]; then
    echo "perf-agent: CAP_SYSLOG is in the bounding set but this image's binary does not" >&2
    echo "  carry it as a file capability, so kernel frames still will not be symbolized." >&2
fi

exec "${BIN}" "$@"
