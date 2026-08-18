# One measurement pass, as a NixOS test.
#
# The guest builds a pool, fills the ARC, squeezes it, drops the caches and
# compacts, taking a snapshot of every pageblock at each step. The snapshots
# land in $out and a separate derivation compares them, so a run is cached on
# its own and CI reruns only what changed.
#
# The load is many small files rather than one big one. Object count is what
# grows the caches that hold pageblocks hostage: dnode_t per dnode,
# dmu_buf_impl_t per dbuf, arc_buf_hdr_t_full per buffer. One large file gives
# the ARC plenty of data and leaves those caches nearly empty, and then there is
# nothing for the patches to act on. The set is deliberately larger than guest
# memory so the ARC has to evict rather than hold everything at once.
{
  pkgs,
  lib,
  variants,
  fragload,
}:

{
  variant,
  seed ? 1,
  recordSize ? "128k",
  compression ? "off",
  readJobs ? 0,
  burstJobs ? 0,
  compactWhileWarm ? false,
  hugeDemand ? 0,
  compactRounds ? 12,
  compactSeconds ? 90,
  files ? 50000,
  fileSize ? 131072,
  memoryMB ? 6144,
  cores ? 4,
  diskMB ? 24000,
  arcFloorMB ? 512,
  quietSeconds ? 60,
  hotFiles ? 2048,
  defragMode ? null,
}:

let
  name =
    "${variant}-${recordSize}-${compression}-seed${toString seed}"
    + lib.optionalString compactWhileWarm "-compactwarm";
  jobsArg = lib.optionalString (readJobs > 0) " -jobs ${toString readJobs}";

  # The read pass ends with memory full and the high orders nearly gone, which
  # is the state a cache growing through vmalloc has to be asked to grow in.
  # Right after the squeeze it was asked with a thousand order 10 blocks free,
  # and any request succeeded. A different seed so the set is cold again.
  burstCmd =
    "fragload -mode read -dir /tank/data/set"
    + " -files ${toString files} -size ${toString fileSize}"
    + " -seed ${toString (seed + 1)} -jobs ${toString burstJobs}";
  setBytes = files * fileSize;
in
pkgs.testers.runNixOSTest {
  name = "fragmentation-${name}";

  nodes.machine = {
    virtualisation = {
      memorySize = memoryMB;
      inherit cores;
      diskSize = 4096;
      emptyDiskImages = [ diskMB ];
    };

    boot = {
      kernelPackages = variants.${variant};
      supportedFilesystems = [ "zfs" ];
      extraModulePackages = [ variants.${variant}.slabwho ];
      kernel.sysctl = lib.optionalAttrs (defragMode != null) {
        "vm.defrag_mode" = defragMode;
      };
    };

    networking.hostId = "deadbeef";
    environment.systemPackages = [ fragload ];

    documentation.enable = false;
    services.udisks2.enable = false;
  };

  testScript = ''
    import os
    from datetime import timedelta

    machine.start()
    machine.wait_for_unit("multi-user.target")

    machine.succeed("modprobe zfs")
    machine.succeed("modprobe slabwho")
    machine.succeed("zpool create -f -o ashift=12 tank /dev/vdb")
    machine.succeed(
        "zfs create -o recordsize=${recordSize} -o compression=${compression}"
        " -o atime=off tank/data"
    )

    # ZFS keeps its own tuning: holding the ARC down leaves memory free, and
    # then the allocator never runs short of the high orders being measured.
    ceiling = int(machine.succeed("awk '$1==\"c_max\"{print $3}' /proc/spl/kstat/zfs/arcstats"))

    machine.succeed("echo 0 > /sys/module/zfs/parameters/zfs_bclone_enabled")

    machine.succeed(
        "fragload -mode write -dir /tank/data/set"
        " -files ${toString files} -size ${toString fileSize} -seed ${toString seed}",
        timeout=timedelta(seconds=1800),
    )
    machine.succeed("sync")

    # Holes or clones here would make every number after this point worthless.
    alloc = int(machine.succeed("zpool list -Hp -o alloc tank"))
    want = ${toString setBytes}
    assert alloc > want - want // 8, f"only {alloc} bytes on disk for a {want} byte set"

    machine.succeed("echo 3 > /proc/sys/vm/drop_caches")

    def snapshot(phase):
        d = f"/tmp/proc/{phase}"
        machine.succeed(f"mkdir -p {d}/spl/kstat/zfs {d}/sys/kernel")
        for f in ("iomem", "buddyinfo", "pagetypeinfo", "slabinfo", "meminfo", "vmstat"):
            machine.succeed(f"cat /proc/{f} > {d}/{f}")
        machine.succeed(f"cat /proc/spl/kstat/zfs/arcstats > {d}/spl/kstat/zfs/arcstats")
        # Chunk orders and the relocation counters. Without these there is no
        # way to tell whether a run exercised the compound path at all, and a
        # guest small enough for a runner may only ever produce order 0.
        machine.succeed(f"cat /proc/spl/kstat/zfs/abdstats > {d}/spl/kstat/zfs/abdstats")
        machine.succeed(f"cat /proc/spl/kmem/slab > {d}/spl/kmem-slab")
        for f in ("hostname", "osrelease"):
            machine.succeed(f"cat /proc/sys/kernel/{f} > {d}/sys/kernel/{f}")
        machine.succeed(f"cat /proc/slabwho > {d}/slabwho || true")
        # Which caches SLUB folded together, so a name in the report is never
        # taken for the only thing living in that cache.
        machine.succeed(
            f"find /sys/kernel/slab -maxdepth 1 -type l -printf '%f %l\\n' > {d}/slabmerge"
        )
        for f in ("kpageflags", "kpagecount"):
            machine.succeed(f"gzip -1 -c /proc/{f} > {d}/{f}.gz")

    # A kernel that corrupts a list during migration keeps running afterwards
    # and every phase below still passes, so the run has to be told to look.
    # Printing what it said beats a bare failure: the difference between a
    # double free and a bad list walk is the whole diagnosis.
    def kernel_is_quiet(where):
        said = machine.succeed(
            "dmesg | grep -E 'BUG:|Oops:|kernel BUG at|list_del corruption"
            "|list_add corruption|refcount_t' || true"
        )
        assert not said.strip(), f"the kernel complained during {where}:\n{said}"

    # Chunks of more than one page, right now rather than since boot: the
    # counter is bumped on allocation and bumped down on free.
    def compound_chunks():
        return int(machine.succeed(
            "awk '$1 ~ /^scatter_order_[1-9]/ { n += $3 } END { print n+0 }'"
            " /proc/spl/kstat/zfs/abdstats"
        ))

    # Absent on a build without the relocation patches, which reads as zero.
    def abdstat(name):
        return int(machine.succeed(
            f"awk '$1 == \"{name}\" {{ n = $3 }} END {{ print n+0 }}'"
            " /proc/spl/kstat/zfs/abdstats"
        ))

    # The dbuf relocation probe counts in dbufstats. Absent without the probe
    # patch, which reads as zero.
    def dbufstat(name):
        return int(machine.succeed(
            f"awk '$1 == \"{name}\" {{ n = $3 }} END {{ print n+0 }}'"
            " /proc/spl/kstat/zfs/dbufstats"
        ))

    # Seeded shuffle: read in order, the prefetcher answers most of it.
    with subtest("warm"):
        machine.succeed(
            "fragload -mode read -dir /tank/data/set"
            " -files ${toString files} -size ${toString fileSize} -seed ${toString seed}"
            "${jobsArg}",
            timeout=timedelta(seconds=1800),
        )
        snapshot("warm")

    # Compaction has to run while the ARC still holds the chunks it allocated.
    # The compact phase further down runs after the squeeze and a cache drop,
    # by which point almost nothing of ours is left to move, which is why it
    # never exercised relocation however often it ran.
    if ${if compactWhileWarm then "True" else "False"}:
        with subtest("compact-warm"):
            compound = compound_chunks()
            assert compound > 1000, (
                f"only {compound} chunks larger than a page are allocated, so"
                " compaction has nothing of ours to move and passing here"
                " would prove nothing"
            )

            # kcompactd, not just the synchronous pass a sysctl write drives.
            # The report this reproduces came from the background daemon, and
            # it runs in a different migration mode against a different target
            # order, so driving only the sysctl leaves that path untouched.
            machine.succeed("echo 100 > /proc/sys/vm/compaction_proactiveness")

            # Readers in flight. Relocation refuses a chunk that is being read
            # and that refusal is a path of its own; on the machine this came
            # from, exactly one chunk was refused that way before it died.
            # Without load the guest compacts an ARC nothing is touching.
            machine.succeed(
                "systemd-run --unit=abd-readers --collect"
                " fragload -mode hot -dir /tank/data/set"
                " -files ${toString files} -size ${toString fileSize}"
                " -seed ${toString seed} -slice ${toString hotFiles}"
                " -secs ${toString compactSeconds}"
            )

            # Deliberately not succeed(). Writing here runs compaction in the
            # caller's own context, so the corruption being looked for kills
            # this shell, and a dead shell reads as a failed command rather
            # than as what the kernel actually said about it.
            for round in range(${toString compactRounds}):
                # A genuine high order request. This is what wakes the daemon
                # on a machine that is merely short of contiguous memory, and
                # it carries a target order, which decides a branch that both
                # proactive compaction and the sysctl skip by passing -1.
                machine.execute("echo 512 > /proc/sys/vm/nr_hugepages")
                machine.execute("echo 1 > /proc/sys/vm/compact_memory")
                machine.sleep(duration=timedelta(seconds=5))
                machine.execute("echo 0 > /proc/sys/vm/nr_hugepages")
                kernel_is_quiet(f"compaction round {round} against a busy ARC")

            machine.succeed("systemctl stop abd-readers || true")
            kernel_is_quiet("compaction with a full ARC")

            asked = abdstat("page_isolate_asked")
            moved = abdstat("page_migrated")
            busy = abdstat("page_migrate_busy")
            lost = abdstat("page_migrate_lost")
            waited = abdstat("gate_waited")
            woke = machine.succeed(
                "awk '$1 == \"compact_daemon_wake\" { print $2 }' /proc/vmstat"
            ).strip()
            print(
                f"compound chunks {compound}, offered {asked}, moved {moved},"
                f" refused busy {busy}, lost {lost}, gate waited {waited},"
                f" kcompactd woke {woke}"
            )
            assert asked > 0, (
                "compaction never offered a chunk for relocation, so the path"
                " under test did not run and this proves nothing"
            )
            snapshot("warm-compacted")

    # What the patches are for, asked as a question the allocator answers in
    # one number: with the ARC holding its chunks, how much contiguous memory
    # can the machine still hand out? Counted while the pages are held, since
    # releasing them first would leave nothing to count.
    if ${toString hugeDemand} > 0:
        with subtest("highorder"):
            before = compound_chunks()
            machine.succeed("echo ${toString hugeDemand} > /proc/sys/vm/nr_hugepages")
            got = int(machine.succeed("awk '/^HugePages_Total/{print $2}' /proc/meminfo"))
            print(
                f"asked ${toString hugeDemand}, got {got},"
                f" compound chunks {before},"
                f" retire waited {abdstat('retire_waited')}"
                f" longest {abdstat('retire_spin_max')},"
                f" gate waited {abdstat('gate_waited')}"
            )
            assert before > 1000, (
                f"only {before} chunks larger than a page, so the ARC is not"
                " holding the memory this is meant to compete with"
            )
            # The retire path spins rather than sleeps, so what matters is not
            # how much it waited in total but how long it ever waited at once.
            # A few hundred turns is microseconds; a run into the thousands
            # would mean it is waiting on something that sleeps, and that is
            # the shape worth failing on.
            longest = abdstat("retire_spin_max")
            assert longest < 10000, (
                f"one retire spun {longest} times without giving up the cpu,"
                " which is a stall rather than a wait"
            )

            snapshot("highorder")
            machine.succeed("echo 0 > /proc/sys/vm/nr_hugepages")
            kernel_is_quiet("the high order demand")

    with subtest("squeeze"):
        machine.succeed(
            "echo ${toString (arcFloorMB * 1024 * 1024)} > /sys/module/zfs/parameters/zfs_arc_max"
        )
        machine.succeed(
            "fragload -mode hot -dir /tank/data/set"
            " -files ${toString files} -size ${toString fileSize} -seed ${toString seed}"
            " -slice ${toString hotFiles} -secs ${toString quietSeconds}",
            timeout=timedelta(seconds=1800),
        )
        snapshot("squeezed")

    with subtest("drop"):
        machine.succeed("sync; echo 3 > /proc/sys/vm/drop_caches")
        machine.sleep(duration=timedelta(seconds=5))
        snapshot("dropped")

    # Object mobility only acts inside compaction, and nothing above asks for it.
    with subtest("compact"):
        # The dbuf probe answers inside compaction, so read its counters as a
        # delta across this one pass rather than since boot. The reasons are
        # why a dbuf could not be relocated; move_would is the share that
        # could, and is the whole point of the measurement.
        reasons = [
            "move_offered", "move_would", "move_no_lock", "move_stale",
            "move_bonus", "move_held", "move_user", "move_dirty", "move_state",
        ]
        before = {r: dbufstat(r) for r in reasons}

        machine.succeed("echo 1 > /proc/sys/vm/compact_memory")
        machine.sleep(duration=timedelta(seconds=10))

        delta = {r: dbufstat(r) - before[r] for r in reasons}
        if delta["move_offered"] > 0:
            print("dbuf probe (this compaction pass):")
            for r in reasons:
                print(f"  {r:14s} {delta[r]}")
            movable = 100 * delta["move_would"] / delta["move_offered"]
            print(f"  -> {movable:.1f}% of offered dbufs were relocatable")
        snapshot("compacted")

    # Memory fragmented and busy, unlike above. Writing zero would not restore
    # the ceiling: arc_c_max stays where the squeeze put it.
    with subtest("reread"):
        machine.succeed(f"echo {ceiling} > /sys/module/zfs/parameters/zfs_arc_max")
        machine.succeed(
            "fragload -mode read -dir /tank/data/set"
            " -files ${toString files} -size ${toString fileSize} -seed ${toString seed}"
            "${jobsArg}",
            timeout=timedelta(seconds=1800),
        )
        ${lib.optionalString (
          burstJobs > 0
        ) ''machine.succeed("${burstCmd}", timeout=timedelta(seconds=1800))''}
        snapshot("reread")

    kernel_is_quiet("the run")

    os.makedirs(os.environ["out"], exist_ok=True)
    machine.copy_from_machine("/tmp/proc", "")
  '';
}
