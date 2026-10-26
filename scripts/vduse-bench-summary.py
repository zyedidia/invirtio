#!/usr/bin/env python3
# Summarize the results of vduse-bench.sh.
#
# usage: vduse-bench-summary.py <output dir>
#
# For each job and target, it prints the median over the repetitions of the
# IOPS, bandwidth, mean latency and clat percentiles, the spread of the IOPS,
# and the device process's CPU time per I/O. For UML targets it then prints
# the vduse_guest counters of each job.

import json
import os
import statistics
import sys

JOBS = ["lat-qd1", "iops-qd32", "iops-qd128", "seqread-128k", "seqread-1m",
        "seqwrite-128k", "seqwrite-1m"]
COUNTERS = ["kicks", "kick_irqs", "requests", "injected", "suppressed",
            "bad_requests", "inject_errors", "dropped_kicks", "polled",
            "poll_timeouts"]

if len(sys.argv) != 2:
    sys.exit(f"usage: {os.path.basename(sys.argv[0])} <output dir>")
OUT = sys.argv[1]

host = {}
with open(f"{OUT}/host") as f:
    for line in f:
        p = line.split()
        for i in range(0, len(p) - 1, 2):
            host[p[i]] = p[i + 1]
HZ = int(host.get("clk_tck", 100))
REPS = int(host.get("reps", 3))
with open(f"{OUT}/targets") as f:
    TARGETS = f.read().split()


def load(target, job, rep):
    with open(f"{OUT}/{target}/{job}-{rep}.json") as f:
        j = json.load(f)["jobs"][0]
    return (j["read"] if j["read"]["io_bytes"] else j["write"]), j


# Return the counters of queue 0 in a /proc/vduse_guest dump.
def queue_counters(path):
    out = {}
    queue = None
    with open(path) as f:
        for line in f:
            p = line.split()
            if len(p) == 2 and p[0] == "queue":
                queue = p[1]
            elif len(p) == 2 and queue == "0" and p[1].isdigit():
                out[p[0]] = int(p[1])
    return out


def med(xs):
    return statistics.median(xs) if xs else 0


for job in JOBS:
    print(f"{job}")
    print(f"  {'target':10s} {'IOPS':>9s} {'spread':>7s} {'MiB/s':>8s}"
          f" {'mean':>8s} {'p50':>8s} {'p99':>8s} {'cpu/io':>8s}"
          f" {'usr':>7s} {'sys':>7s}")
    for t in TARGETS:
        r = {k: [] for k in ("iops", "bw", "mean", "p50", "p99", "usr", "sys")}
        for rep in range(1, REPS + 1):
            d, j = load(t, job, rep)
            pct = d["clat_ns"]["percentile"]
            r["iops"].append(d["iops"])
            r["bw"].append(d["bw_bytes"] / 2**20)
            r["mean"].append(d["lat_ns"]["mean"] / 1000)
            r["p50"].append(pct["50.000000"] / 1000)
            r["p99"].append(pct["99.000000"] / 1000)
            ticks = f"{OUT}/{t}/{job}-{rep}.ticks"
            if os.path.exists(ticks):
                u0, s0, u1, s1 = map(int, open(ticks).read().split())
                # The ticks also cover the 1-second ramp.
                runtime = j["job_runtime"]
                ios = d["total_ios"] * (runtime + 1000) / runtime
                r["usr"].append(1e6 * (u1 - u0) / HZ / ios)
                r["sys"].append(1e6 * (s1 - s0) / HZ / ios)
        iops = r["iops"]
        spread = 100 * (max(iops) - min(iops)) / med(iops)
        line = (f"  {t:10s} {med(iops):9.0f} {spread:6.1f}%"
                f" {med(r['bw']):8.0f} {med(r['mean']):8.1f}"
                f" {med(r['p50']):8.1f} {med(r['p99']):8.1f}")
        if r["usr"]:
            cpu = [u + s for u, s in zip(r["usr"], r["sys"])]
            line += (f" {med(cpu):8.2f} {med(r['usr']):7.2f}"
                     f" {med(r['sys']):7.2f}")
        print(line)
    print()

print("Times are in microseconds. cpu/io is the device process's CPU time")
print("per I/O, split into user and system time.")

for t in TARGETS:
    if not os.path.exists(f"{OUT}/{t}/{JOBS[0]}-1.before"):
        continue
    print()
    print(f"{t} counters of queue 0, median over the repetitions")
    print(f"  {'job':14s} " + " ".join(f"{c:>13s}" for c in COUNTERS))
    for job in JOBS:
        rows = []
        for rep in range(1, REPS + 1):
            a = queue_counters(f"{OUT}/{t}/{job}-{rep}.after")
            b = queue_counters(f"{OUT}/{t}/{job}-{rep}.before")
            # Guests from before a counter existed report it as 0.
            rows.append({k: a.get(k, 0) - b.get(k, 0) for k in COUNTERS})
        print(f"  {job:14s} " +
              " ".join(f"{med([r[c] for r in rows]):13.0f}" for c in COUNTERS))
