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
  name = "${variant}-${recordSize}-seed${toString seed}";
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
        "zfs create -o recordsize=${recordSize} -o compression=off -o atime=off tank/data"
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
        for f in ("hostname", "osrelease"):
            machine.succeed(f"cat /proc/sys/kernel/{f} > {d}/sys/kernel/{f}")
        machine.succeed(f"cat /proc/slabwho > {d}/slabwho || true")
        for f in ("kpageflags", "kpagecount"):
            machine.succeed(f"gzip -1 -c /proc/{f} > {d}/{f}.gz")

    # Seeded shuffle: read in order, the prefetcher answers most of it.
    with subtest("warm"):
        machine.succeed(
            "fragload -mode read -dir /tank/data/set"
            " -files ${toString files} -size ${toString fileSize} -seed ${toString seed}",
            timeout=timedelta(seconds=1800),
        )
        snapshot("warm")

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
            " -files ${toString files} -size ${toString fileSize} -seed ${toString seed}",
            timeout=timedelta(seconds=1800),
        )
        snapshot("reread")

    os.makedirs(os.environ["out"], exist_ok=True)
    machine.copy_from_machine("/tmp/proc", "")
  '';
}
