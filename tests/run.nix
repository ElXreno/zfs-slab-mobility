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
  cloneWhileWarm ? false,
  cloneJobs ? 4,
  cloneSeconds ? 90,
  # The machine this reproduces has an encrypted pool, dedup on the dataset
  # being cloned into, and a snapshot every fifteen minutes. Each of those puts
  # code in the path that a plain pool never runs, so each is a knob rather
  # than an assumption.
  encrypted ? false,
  dedupDest ? false,
  snapshotWhileCloning ? false,
  writeWhileCloning ? false,
  # Whether cloning waits for a dirty block's transaction group or hands the
  # caller a shortened range to fall back on. The two take different paths out
  # of zfs_clone_range, and only one of them has ever been exercised here.
  bcloneWaitDirty ? null,
  # Refuses header relocation without reading the header, so a run that still
  # goes wrong says the trouble is not there.
  arcMoveDisable ? false,
  # Which caches carry a relocation callback and which do not. Two of them are
  # unmovable by construction rather than by choice, and nothing else in the
  # suite would notice a callback being added to one of those by mistake.
  assertMobility ? false,
  hugeDemand ? 0,
  arcFreezeMB ? 3072,
  compactRounds ? 12,
  compactSeconds ? 90,
  files ? 50000,
  fileSize ? 131072,
  memoryMB ? 6144,
  # One vCPU unless a run needs contention: per-CPU allocator lists on four
  # cost separation eleven percent of spread between two runs, one costs two.
  cores ? 1,
  diskMB ? 24000,
  arcFloorMB ? 512,
  quietSeconds ? 60,
  hotFiles ? 2048,
  defragMode ? null,
}:

let
  name =
    "${variant}-${recordSize}-${compression}-seed${toString seed}"
    + lib.optionalString compactWhileWarm "-compactwarm"
    + lib.optionalString cloneWhileWarm "-clonewarm";
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
# Never substituted: a run is a sample, not a function of its inputs.
(pkgs.testers.runNixOSTest {
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
      # The guest's own dice: kernel placement and khugepaged. The allocator's are in nix/variants.nix.
      kernelParams = [
        "nokaslr"
        "transparent_hugepage=never"
      ];
      kernel.sysctl = {
        "kernel.randomize_va_space" = 0;
      }
      // lib.optionalAttrs (defragMode != null) {
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
    if ${if arcMoveDisable then "True" else "False"}:
        machine.succeed("echo 1 > /sys/module/zfs/parameters/zfs_arc_move_disable")
    machine.succeed("modprobe slabwho")
    # Not a secret and not pretending to be one: the point is that the pool
    # runs the encrypted read path, where a dnode block has to be decrypted
    # before anything can be read out of it.
    if ${if encrypted then "True" else "False"}:
        machine.succeed("echo stand-not-a-secret > /run/zfskey")
    machine.succeed(
        "zpool create -f -o ashift=12"
        "${lib.optionalString encrypted " -O encryption=aes-256-gcm -O keyformat=passphrase -O keylocation=file:///run/zfskey"}"
        " tank /dev/vdb"
    )
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

    # A snapshot taken while kswapd, kcompactd, the evict thread or a
    # transaction group is still at work records a moment, and two runs never
    # share a moment. Wait until the counters those actors move have stood
    # still for a second. Bounded: a guest that never settles is reported,
    # not waited for.
    def settle(where, seconds=30):
        machine.succeed("sync; zpool sync tank")
        probe = (
            "awk '$1 ~ /^(compact_stall|compact_daemon_wake|compact_isolated"
            "|pgscan_kswapd|pgsteal_kswapd|pgscan_direct|pgmigrate_success"
            "|pgmigrate_fail)$/ { s = s $2 \",\" } END { print s }' /proc/vmstat;"
            " awk '$1 ~ /^(memory_direct_count|memory_indirect_count"
            "|evict_skip)$/ { s = s $3 \",\" } END { print s }'"
            " /proc/spl/kstat/zfs/arcstats"
        )
        last = None
        for _ in range(seconds):
            now = machine.succeed(probe)
            if now == last:
                return
            last = now
            machine.sleep(duration=timedelta(seconds=1))
        print(f"{where}: memory was still moving after {seconds}s")

    def snapshot(phase):
        settle(phase)
        d = f"/tmp/proc/{phase}"
        machine.succeed(f"mkdir -p {d}/spl/kstat/zfs {d}/sys/kernel")
        for f in ("iomem", "buddyinfo", "pagetypeinfo", "slabinfo", "meminfo", "vmstat"):
            machine.succeed(f"cat /proc/{f} > {d}/{f}")
        machine.succeed(f"cat /proc/spl/kstat/zfs/arcstats > {d}/spl/kstat/zfs/arcstats")
        # Chunk orders and the relocation counters. Without these there is no
        # way to tell whether a run exercised the compound path at all, and a
        # guest small enough for a runner may only ever produce order 0.
        machine.succeed(f"cat /proc/spl/kstat/zfs/abdstats > {d}/spl/kstat/zfs/abdstats")
        machine.succeed(f"cat /proc/spl/kstat/zfs/dbufstats > {d}/spl/kstat/zfs/dbufstats")
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

    # Whatever the diagnostic patch said. Nothing else records the failure
    # being hunted: it reaches userspace as a bare EIO with no ereport and no
    # entry in the pool's error log, so this ring buffer is the only witness.
    def zfs_said(pattern):
        return machine.succeed(
            f"grep -E '{pattern}' /proc/spl/kstat/zfs/dbgmsg || true"
        ).strip()

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

    if ${if assertMobility then "True" else "False"}:
        with subtest("cache-mobility"):
            # slabwho column six: whether the cache was created with a move
            # callback. A cache absent from the file reads as zero, which is
            # why the movable ones are asserted too: a typo in a name would
            # otherwise pass as "not mobile".
            def cache_mobile(name):
                out = machine.succeed(
                    f"awk '$1 == \"{name}\" {{ print $6; found=1 }}"
                    f" END {{ if (!found) print \"absent\" }}' /proc/slabwho"
                ).strip()
                return out

            for name in ("dnode_t", "arc_buf_hdr_t_full"):
                got = cache_mobile(name)
                assert got == "1", (
                    f"{name} should carry a relocation callback, slabwho says"
                    f" {got}"
                )

            # Proved unmovable by reading the code: a znode contains the VFS
            # inode the kernel reaches it through, and zio_buf_alloc hands out
            # a bare pointer nothing tracks. dmu_buf_impl_t is movable in
            # principle but its probe belongs to the probes variant, so the
            # build a machine would run must not carry it.
            for name in ("zfs_znode_cache", "zio_buf_comb_16384",
                         "dmu_buf_impl_t"):
                got = cache_mobile(name)
                assert got in ("0", "absent"), (
                    f"{name} must not carry a relocation callback, slabwho"
                    f" says {got}"
                )

        # hdr_size is the bytes the two header caches hold and nothing else.
        # A build that constructs an object when its slab is created, on top
        # of constructing it when it is handed out, counts every slot once
        # more than it ever returns, and the ARC then evicts against memory
        # it does not hold. Both numbers in one command, so that the churn
        # between two reads cannot be mistaken for the defect.
        with subtest("arc-accounting"):
            said = machine.succeed(
                "awk '$1 == \"hdr_size\" { print $3 }' /proc/spl/kstat/zfs/arcstats;"
                " awk '$1 == \"arc_buf_hdr_t_full\" || $1 == \"arc_buf_t\" { n += $4 }"
                " END { print n+0 }' /proc/spl/kmem/slab"
            ).split()
            hdr_size, held = int(said[0]), int(said[1])
            assert abs(hdr_size - held) <= max(held // 10, 1 << 20), (
                f"hdr_size says {hdr_size} bytes, the header caches hold {held}"
            )

    # A build clones every file it installs from the build directory into the
    # store, and on a machine where both live in one pool that clone reads the
    # source's indirect blocks. One such read came back EIO with nothing
    # anywhere to say why. The suspicion is that relocation moved something
    # out from under it, so the clones have to run while compaction is
    # actually moving pages: an earlier attempt ran six hundred of them on a
    # quiet machine and proved nothing.
    if ${if cloneWhileWarm then "True" else "False"}:
        with subtest("clone-warm"):
            machine.succeed(
                "zfs create -o recordsize=${recordSize}"
                " -o compression=${compression} -o atime=off"
                "${lib.optionalString dedupDest " -o dedup=blake3"}"
                " tank/clone"
            )
            # Off for the write phase above, because a clone there would leave
            # the set sharing blocks and the size check would not mean what it
            # says. On now, which is the whole point of this phase.
            machine.succeed("echo 1 > /sys/module/zfs/parameters/zfs_bclone_enabled")
            ${lib.optionalString (bcloneWaitDirty != null) ''
              machine.succeed(
                  "echo ${toString bcloneWaitDirty} >"
                  " /sys/module/zfs/parameters/zfs_bclone_wait_dirty"
              )''}

            machine.succeed(
                "systemd-run --unit=clone-load"
                " fragload -mode clone -dir /tank/data/set -dest /tank/clone"
                " -files ${toString files} -size ${toString fileSize}"
                " -jobs ${toString cloneJobs} -secs ${toString cloneSeconds}"
            )

            # A build does not stop compiling while it installs. Both earlier
            # attempts cloned against an otherwise idle pool, which leaves out
            # every path that needs a write in flight: a block with a pending
            # clone written before the txg syncs goes to DB_UNCACHED, and
            # anyone waiting on it wakes to EIO.
            if ${if writeWhileCloning then "True" else "False"}:
                machine.succeed(
                    "systemd-run --unit=write-load"
                    " fragload -mode write -dir /tank/clone/churn"
                    " -files 4096 -size ${toString fileSize}"
                    " -seed ${toString (seed + 7)} -jobs 4"
                )

            # The machine that hit this takes one every fifteen minutes, and
            # one landed seven minutes before the failure. A snapshot ends a
            # transaction group, which is the boundary cloning cares about.
            if ${if snapshotWhileCloning then "True" else "False"}:
                # Absolute paths throughout: a systemd-run unit gets none of
                # the login shell's PATH, and a missing sleep turns a paced
                # loop into a spin that starves the load it was meant to
                # accompany.
                machine.succeed(
                    "systemd-run --unit=snap-load --property=Type=simple"
                    " /bin/sh -c 'i=0; while true; do"
                    " zfs snapshot tank/data@s$i 2>/dev/null;"
                    " zfs destroy tank/data@s$((i-8)) 2>/dev/null;"
                    " i=$((i+1));"
                    " /run/current-system/sw/bin/sleep 2; done'"
                )

            # Same three levers that made the last relocation bug show itself:
            # a real high order request rather than the sysctl alone, repeated
            # rounds, and load in flight the whole time.
            for round in range(${toString compactRounds}):
                machine.execute("echo 512 > /proc/sys/vm/nr_hugepages")
                machine.execute("echo 1 > /proc/sys/vm/compact_memory")
                machine.sleep(duration=timedelta(seconds=5))
                machine.execute("echo 0 > /proc/sys/vm/nr_hugepages")
                kernel_is_quiet(f"clone round {round}")

            machine.wait_until_fails(
                "systemctl is-active clone-load",
                timeout=timedelta(seconds=${toString (cloneSeconds + 120)}),
            )
            if ${if snapshotWhileCloning then "True" else "False"}:
                machine.succeed("systemctl stop snap-load || true")
            if ${if writeWhileCloning then "True" else "False"}:
                machine.succeed("systemctl stop write-load || true")
            status = machine.succeed(
                "systemctl show -p ExecMainStatus --value clone-load"
            ).strip()
            said = machine.succeed(
                "journalctl -u clone-load --no-pager -o cat || true"
            ).strip()
            print(f"clone unit exited {status}: {said}")

            # The diagnostic patch names the block whenever a read dies on the
            # way out of the dbuf layer, whether or not the clone noticed.
            diag = zfs_said("indirect read failed|woke to DB_UNCACHED"
                            "|dbuf_hold gave nothing")
            if diag:
                print(f"zfs said:\n{diag}")

            kernel_is_quiet("cloning against a moving ARC")
            assert status == "0", (
                f"cloning failed while relocation was running (exit {status}):"
                f"\n{said}\nzfs said:\n{diag or '(nothing)'}"
            )
            snapshot("clone-warm")

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

            # The dbuf relocation probe answers here, where compaction runs
            # against a full ARC, not in the later compact phase where the ARC
            # has been squeezed and there are few dbufs left to offer. Read as
            # a delta across the rounds below: the reasons are why a dbuf could
            # not be moved, and move_would is the share that could, which is
            # the whole question. Absent without the probe patch, reads zero.
            dbuf_reasons = [
                "move_offered", "move_would", "move_no_lock", "move_stale",
                "move_bonus", "move_held", "move_user", "move_dirty",
                "move_state",
            ]
            dbuf_before = {r: dbufstat(r) for r in dbuf_reasons}

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

            dbuf_delta = {r: dbufstat(r) - dbuf_before[r] for r in dbuf_reasons}
            if dbuf_delta["move_offered"] > 0:
                print("dbuf probe, across the compaction rounds:")
                for r in dbuf_reasons:
                    print(f"  {r:14s} {dbuf_delta[r]}")
                share = 100 * dbuf_delta["move_would"] / dbuf_delta["move_offered"]
                print(f"  -> {share:.1f}% of offered dbufs were relocatable")

            asked = abdstat("page_isolate_asked")
            moved = abdstat("page_migrated")
            busy = abdstat("page_migrate_busy")
            lost = abdstat("page_migrate_lost")
            waited = abdstat("gate_waited")
            # Anything but zero here is a chunk that reached relocation
            # holding fewer references than this code keeps on one it owns.
            # The guard declines the put rather than freeing a page the
            # caller still holds locked, so the run survives it, but the
            # count is the only trace left and it belongs in the report.
            short = abdstat("migrate_ref_short")
            woke = machine.succeed(
                "awk '$1 == \"compact_daemon_wake\" { print $2 }' /proc/vmstat"
            ).strip()
            print(
                f"compound chunks {compound}, offered {asked}, moved {moved},"
                f" refused busy {busy}, lost {lost}, gate waited {waited},"
                f" refs short {short}, kcompactd woke {woke}"
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
            # Both builds hold the same ARC and kswapd may not shrink it, so
            # the count is about the layout of what is free, not about eviction.
            freeze = ${toString (arcFreezeMB * 1024 * 1024)}
            machine.succeed(f"echo {freeze} > /sys/module/zfs/parameters/zfs_arc_max")
            # A lower ceiling evicts nothing by itself: the ARC gives way on
            # the next allocation, so read a little until it has.
            for _ in range(60):
                size = int(machine.succeed("awk '$1==\"size\"{print $3}' /proc/spl/kstat/zfs/arcstats"))
                if size <= freeze:
                    break
                machine.succeed(
                    "fragload -mode hot -dir /tank/data/set"
                    " -files ${toString files} -size ${toString fileSize}"
                    " -seed ${toString seed} -slice 256 -secs 2",
                    timeout=timedelta(seconds=60),
                )
            assert size <= freeze + freeze // 20, (
                f"the ARC is still {size} bytes against a ceiling of {freeze}"
            )
            machine.succeed("echo 1 > /sys/module/zfs/parameters/zfs_arc_shrinker_limit")
            machine.succeed("echo 1 > /proc/sys/vm/compact_memory")
            snapshot("frozen")
            free10 = machine.succeed("awk '{ n += $NF } END { print n+0 }' /proc/buddyinfo").strip()
            free = int(machine.succeed("awk '/^MemFree/{print $2}' /proc/meminfo")) * 1024
            print(f"frozen: ARC {size}, free {free}, order-10 free blocks {free10}")
            want = ${toString hugeDemand} * 2 * 1024 * 1024
            assert free >= want, (
                f"{free} bytes free against a demand of {want}: the demand"
                " could only be met by evicting, which is not what is measured"
            )
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
            machine.succeed("echo 0 > /sys/module/zfs/parameters/zfs_arc_shrinker_limit")
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
        machine.succeed("echo 1 > /proc/sys/vm/compact_memory")
        machine.sleep(duration=timedelta(seconds=10))
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
}).overrideTestDerivation
  (_: {
    allowSubstitutes = false;
  })
