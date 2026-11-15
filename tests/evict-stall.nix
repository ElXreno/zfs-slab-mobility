# ARC eviction against compaction of ARC folios must not wait for each other forever.
{
  pkgs,
  lib,
  variants,
  fragload,
  fragcheck,
}:

{
  variant,
  seed ? 1,
  memoryMB ? 8192,
  cores ? 12,
  evictThreads ? 4,
  sublists ? 0,
  files ? 1000000,
  bigGB ? 16,
  dbGB ? 6,
  readJobs ? 8,
  dbJobs ? 8,
  walkJobs ? 24,
  compactors ? 2,
  hogMB ? 1536,
  seconds ? 900,
  stall ? 30,
  bigRecord ? "128k",
  expectStall ? false,
}:

let
  name =
    "evict-stall-${variant}"
    + lib.optionalString (sublists > 0) "-sub${toString sublists}"
    + lib.optionalString (bigRecord != "128k") "-rs${bigRecord}"
    + "-seed${toString seed}";

  py = b: if b then "True" else "False";
in
(pkgs.testers.runNixOSTest {
  inherit name;

  nodes.machine = {
    virtualisation = {
      memorySize = memoryMB;
      inherit cores;
      emptyDiskImages =
        map
          (size: {
            inherit size;
            driveConfig.driveExtraOpts.cache = "none";
          })
          [
            32768
            2048
          ];
    };

    boot = {
      kernelPackages = variants.${variant};
      supportedFilesystems = [ "zfs" ];
      kernelParams = [
        "nokaslr"
        "zswap.enabled=1"
      ];
      kernel.sysctl = {
        "vm.panic_on_oom" = lib.mkForce 0;
        "vm.compaction_proactiveness" = 100;
      };
      extraModprobeConfig =
        "options zfs zfs_arc_evict_threads=${toString evictThreads}"
        + lib.optionalString (sublists > 0) " zfs_multilist_num_sublists=${toString sublists}";
    };

    networking.hostId = "deadbeef";
    environment.systemPackages = [
      fragload
      fragcheck
      pkgs.fio
      pkgs.dua
    ];

    documentation.enable = false;
    services.udisks2.enable = false;
  };

  testScript = ''
    import json
    import os
    import time

    BIN = "/run/current-system/sw/bin"
    out = os.environ["out"]
    os.makedirs(out, exist_ok=True)

    STACKS = r"""
    for t in /proc/[0-9]*/task/[0-9]*; do
      c=$(cat $t/comm 2>/dev/null) || continue
      s=$(awk '/^State:/{print $2}' $t/status 2>/dev/null)
      case "$s:$c" in
        D:*|*:arc_evict*|*:kswapd*|*:kcompactd*|*:arc_prune*|*:arc_reap*|*:txg_sync*|*:z_rd_int*|*:dbu_evict*)
          echo "== $t $c $s"; cat $t/stack;;
      esac
    done
    """

    SUSPECTS = (
        "grep -lE 'arc_wait_for_eviction|abd_free_chunks|arc_lru_evict'"
        " /proc/[0-9]*/task/[0-9]*/stack 2>/dev/null || true"
    )

    VMSTAT = (
        "compact_stall", "compact_fail", "compact_success", "compact_daemon_wake",
        "compact_isolated", "compact_migrate_scanned", "pgmigrate_success", "pgmigrate_fail",
        "thp_split_page", "allocstall_normal", "allocstall_movable", "pgscan_kswapd", "pgscan_direct",
    )

    def kstat(kind):
        rc, text = machine.execute(f"timeout 10 cat /proc/spl/kstat/zfs/{kind}", timeout=60)
        stats = {}
        if rc != 0:
            return stats
        for line in text.splitlines()[2:]:
            parts = line.split()
            if len(parts) == 3 and parts[2].isdigit():
                stats[parts[0]] = int(parts[2])
        return stats

    def vmstat():
        rc, text = machine.execute("timeout 10 cat /proc/vmstat", timeout=60)
        stats = {}
        if rc != 0:
            return stats
        for line in text.splitlines():
            parts = line.split()
            if len(parts) == 2 and parts[0] in VMSTAT:
                stats[parts[0]] = int(parts[1])
        return stats

    def splits():
        rc, text = machine.execute("timeout 10 cat /sys/kernel/tracing/trace_stat/function*", timeout=60)
        if rc != 0:
            return -1
        return sum(int(l.split()[1]) for l in text.splitlines() if l.split()[:1] == ["__folio_split"])

    def counters():
        merged = {}
        merged.update(vmstat())
        arc = kstat("arcstats")
        abd = kstat("abdstats")
        for k in ("lru_evict", "lru_evict_skip", "lru_evict_held", "lru_evict_busy", "mutex_miss",
                  "evict_skip", "memory_direct_count", "memory_indirect_count"):
            merged[k] = arc.get(k, 0)
        for k, v in abd.items():
            if k.startswith(("lru_", "page_", "scatter_order_")):
                merged["abd_" + k] = v
        merged["folio_split_calls"] = splits()
        return merged

    def suspects():
        found = machine.succeed(SUSPECTS, timeout=120).split()
        if not found:
            return {}
        status = " ".join(p[: -len("stack")] + "status" for p in found)
        text = machine.succeed(f"grep -H ctxt_switches {status} 2>/dev/null || true", timeout=120)
        seen = {}
        for line in text.splitlines():
            path, _, rest = line.partition(":")
            seen[path] = seen.get(path, "") + rest.split()[-1] + " "
        return seen

    def active(unit_name):
        return machine.execute(f"systemctl is-active {unit_name}", timeout=120)[1].strip() in ("active", "activating")

    def unit(unit_name, command):
        machine.succeed(f"systemd-run --unit={unit_name} {BIN}/sh -c '{command}'")

    class Stalled(Exception):
        pass

    watch: dict[str, tuple[str, float]] = {}

    def poll():
        now = suspects()
        stamp = time.time()
        for p in list(watch):
            if p not in now:
                del watch[p]
        for p, sig in now.items():
            if p not in watch or watch[p][0] != sig:
                watch[p] = (sig, stamp)
        held = {p for p, (_, start) in watch.items() if stamp - start >= ${toString stall}}
        if held:
            raise Stalled(held)
        return len(now)

    def guarded(unit_name, command, limit):
        unit(unit_name, command)
        deadline = time.time() + limit
        while active(unit_name):
            assert time.time() < deadline, f"{unit_name} ran longer than {limit} s"
            time.sleep(5)
            poll()
        outcome = machine.succeed(f"systemctl show -p Result --value {unit_name}").strip()
        assert outcome == "success", f"{unit_name} ended with {outcome}"

    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.succeed("mkswap /dev/vdc && swapon /dev/vdc && modprobe zfs")
    threads = machine.succeed("cat /sys/module/zfs/parameters/zfs_arc_evict_threads").strip()
    assert threads == "${toString evictThreads}", f"evict threads stayed at {threads}"
    lists = machine.succeed("cat /sys/module/zfs/parameters/zfs_multilist_num_sublists").strip()
    assert lists == "${toString sublists}", f"sublists stayed at {lists}"
    machine.succeed(
        "echo evictstallkey > /run/key && zpool create -f -o ashift=12"
        " -O encryption=on -O keyformat=passphrase -O keylocation=file:///run/key"
        " -O compression=lz4 -O atime=off -O xattr=sa tank /dev/vdb"
        " && zfs create -o recordsize=${bigRecord} tank/big && zfs create -o recordsize=16k tank/db"
        " && zfs create tank/tree"
    )

    samples: list[dict[str, int]] = []
    before: dict[str, int] = {}
    stuck: set[str] = set()
    phase = "setup"
    rounds = 0
    try:
        guarded(
            "layout-tree",
            f"{BIN}/fragload -mode write -dir /tank/tree -files ${toString files} -size 512 -per-dir 1000"
            " -jobs ${toString cores} -seed ${toString seed}",
            1800,
        )
        for ds, gb in (("big", ${toString bigGB}), ("db", ${toString dbGB})):
            guarded(
                f"layout-{ds}",
                f"{BIN}/fio --name={ds} --filename=/tank/{ds}/file --size={gb}G --rw=write --bs=1M"
                " --ioengine=psync --refill_buffers=1 --end_fsync=1 --output=/dev/null",
                1800,
            )
            used = int(machine.succeed(f"zfs get -Hp -o value used tank/{ds}").strip())
            assert used >= gb * 1024 ** 3 * 9 // 10, f"tank/{ds} compressed down to {used} bytes"
        guarded(
            "reimport",
            f"{BIN}/sync && {BIN}/zpool sync tank && {BIN}/zpool export tank"
            f" && {BIN}/zpool import -d /dev -l tank",
            600,
        )
        machine.execute(
            "cd /sys/kernel/tracing && echo __folio_split > set_ftrace_filter && echo 1 > function_profile_enabled"
        )

        phase = "load"
        runtime = ${toString (seconds + 900)}
        unit(
            "big",
            f"{BIN}/fio --name=big --filename=/tank/big/file --size=${toString bigGB}G --rw=randread"
            f" --bs=${bigRecord} --ioengine=psync --numjobs=${toString readJobs} --time_based"
            f" --runtime={runtime} --group_reporting --output=/run/big.fio",
        )
        unit(
            "db",
            f"{BIN}/fio --name=db --filename=/tank/db/file --size=${toString dbGB}G --rw=randrw --rwmixread=70"
            f" --bs=8k --ioengine=psync --refill_buffers=1 --numjobs=${toString dbJobs} --time_based"
            f" --runtime={runtime} --group_reporting --output=/run/db.fio",
        )
        c_max = kstat("arcstats")["c_max"]
        for _ in range(36):
            if kstat("arcstats").get("size", 0) >= c_max * 6 // 10:
                break
            time.sleep(5)
            poll()
        for i in range(${toString compactors}):
            unit(f"compact{i}", "while :; do echo 1 > /proc/sys/vm/compact_memory; done")
        unit("hog", f"while :; do {BIN}/fragcheck -mode hog -mb ${toString hogMB} -secs 3; {BIN}/sleep 2; done")

        before = counters()
        deadline = time.time() + ${toString seconds}
        while time.time() < deadline:
            if not active(f"walk{rounds}"):
                rounds += 1
                unit(
                    f"walk{rounds}",
                    f"{BIN}/dua -t ${toString walkJobs} aggregate /tank/tree > /dev/null;"
                    " echo 2 > /proc/sys/vm/drop_caches",
                )
            time.sleep(5)
            waiting = poll()
            a = kstat("arcstats")
            samples.append({
                k: a.get(k, 0) for k in ("c", "size", "data_size", "metadata_size", "dnode_size")
            } | {"suspects": waiting, "round": rounds})
    except Stalled as stalled:
        stuck = stalled.args[0]

    stacks = ""
    if stuck:
        stacks = machine.succeed(STACKS, timeout=300)
        print(stacks)
        with open(os.path.join(out, "stacks.txt"), "w") as f:
            f.write(stacks)
        for kind in ("arcstats", "abdstats"):
            with open(os.path.join(out, kind), "w") as f:
                f.write(machine.execute(f"timeout 10 cat /proc/spl/kstat/zfs/{kind}", timeout=60)[1])
        with open(os.path.join(out, "meminfo"), "w") as f:
            f.write(machine.execute("timeout 10 cat /proc/meminfo /proc/vmstat /proc/buddyinfo", timeout=60)[1])

    after = counters()
    result = {
        "name": "${name}",
        "variant": "${variant}",
        "sublists": ${toString sublists},
        "expect_stall": ${py expectStall},
        "verdict": "STALLED" if stuck else "OK",
        "phase": phase,
        "rounds": rounds,
        "seconds": len(samples) * 5,
        "stuck_tasks": len(stuck),
        "blocked_tasks": stacks.count(" D\n"),
        "max_suspects": max((s["suspects"] for s in samples), default=0),
        "c_min": min((s["c"] for s in samples), default=0),
        "size_max": max((s["size"] for s in samples), default=0),
        "delta": {
            k: after[k] - before.get(k, 0)
            for k in sorted(after)
            if not k.startswith("abd_scatter_order_") and after[k] != before.get(k, 0)
        },
        "orders": {
            k[len("abd_scatter_order_"):]: v
            for k, v in sorted(after.items())
            if k.startswith("abd_scatter_order_") and v
        },
        "samples": samples,
    }
    print(json.dumps({k: v for k, v in result.items() if k != "samples"}))
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(result, f, indent=1)

    if stuck:
        machine.crash()
    else:
        machine.succeed("systemctl stop big db hog 'compact*' 'walk*' || true", timeout=300)
    if ${py expectStall}:
        assert stuck, "eviction against compaction did not hang"
    else:
        assert not stuck, f"{len(stuck)} tasks sat in ARC eviction for longer than ${toString stall} s"
  '';
}).overrideTestDerivation
  (_: {
    allowSubstitutes = false;
  })
