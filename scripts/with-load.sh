#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# Runs a command beside N CPU busy loops, so the stability bar of phase 6.1 can
# be applied under load on a runner or this host without installing stress-ng.
# The loops are tracked by PID and are killed when the command ends, fails or the
# script gets INT or TERM; none is ever found by a name pattern, because two
# runners share each VM and a stray loop would starve the other one.
#
# Usage: scripts/with-load.sh N -- command [args...]
#   N is a positive integer or "auto" (the output of nproc).
# Exit status: the command's, 2 for a usage error.
set -euo pipefail

usage() {
  echo "usage: scripts/with-load.sh N -- command [args...]   (N must be a positive integer or auto)" >&2
  exit 2
}

[ "$#" -ge 3 ] || usage
n="$1"
[ "$2" = "--" ] || usage
shift 2

cpus="$(nproc)"
if [ "${n}" = "auto" ]; then
  n="${cpus}"
fi
[[ "${n}" =~ ^[1-9][0-9]*$ ]] || usage

pids=()
cmd_pid=""
cleanup() {
  local pid
  # Only reached with the command still running when a signal arrived.
  if [ -n "${cmd_pid}" ]; then
    kill "${cmd_pid}" 2>/dev/null || true
    wait "${cmd_pid}" 2>/dev/null || true
    cmd_pid=""
  fi
  for pid in "${pids[@]}"; do
    # The loop leads its own session (setsid), so the negative PID reaches it
    # and anything it started; the plain PID covers a loop that did not.
    kill -- "-${pid}" 2>/dev/null || kill "${pid}" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do
    wait "${pid}" 2>/dev/null || true
  done
  pids=()
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for ((i = 0; i < n; i++)); do
  setsid bash -c 'while :; do :; done' &
  pids+=("$!")
done
echo "with-load: ${n} busy loops (pids ${pids[*]}) on ${cpus} CPUs" >&2

# The command runs as a background job that keeps this script's stdin, so a
# signal reaches the trap at once instead of after the command ends.
"$@" <&0 &
cmd_pid=$!
rc=0
wait "${cmd_pid}" || rc=$?
cmd_pid=""
exit "${rc}"
