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
  name = "${variant}-seed${toString seed}";
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
      # Provides /proc/slabwho, which names the cache owning each slab page.
      extraModulePackages = [ variants.${variant}.slabwho ];
      # defrag_mode changes how hard the allocator works to avoid mixing
      # migrate types. Left at the kernel default unless a test pins it.
      kernel.sysctl = lib.optionalAttrs (defragMode != null) {
        "vm.defrag_mode" = defragMode;
      };
    };

    networking.hostId = "deadbeef";
    environment.systemPackages = [ fragload ];

    # Nothing in the guest should compete for memory with the thing being
    # measured, and a snapshot of a machine mid documentation build is noise.
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
        "zfs create -o recordsize=128k -o compression=off -o atime=off tank/data"
    )

    # Half of memory for the ARC, and a dirty limit well below it. At three
    # quarters the write phase has no room left and the guest panics as
    # deadlocked on memory before producing anything.
    allmem = int(machine.succeed("awk '/^MemTotal:/{print $2*1024}' /proc/meminfo"))
    ceiling = allmem // 2
    machine.succeed(f"echo {allmem // 16} > /sys/module/zfs/parameters/zfs_dirty_data_max")
    machine.succeed(f"echo {ceiling} > /sys/module/zfs/parameters/zfs_arc_max")
    got = int(machine.succeed("awk '$1==\"c_max\"{print $3}' /proc/spl/kstat/zfs/arcstats"))
    assert got == ceiling, f"c_max did not take: {got} instead of {ceiling}"

    # Block cloning would answer repeated content by pointing at the same
    # blocks, and then the ARC has a fraction of the data to cache and never
    # grows. The generator writes distinct bytes anyway; this is belt and braces.
    machine.succeed("echo 0 > /sys/module/zfs/parameters/zfs_bclone_enabled")

    machine.succeed(
        "fragload -mode write -dir /tank/data/set"
        " -files ${toString files} -size ${toString fileSize} -seed ${toString seed}",
        timeout=timedelta(seconds=1800),
    )
    machine.succeed("sync")

    # Logical size and allocated size have to agree. When they do not the set is
    # holes or clones, and every number after this point would be worthless.
    alloc = int(machine.succeed("zpool list -Hp -o alloc tank"))
    want = ${toString setBytes}
    assert alloc > want - want // 8, f"only {alloc} bytes on disk for a {want} byte set"

    machine.succeed("echo 3 > /proc/sys/vm/drop_caches")

    # The guest hands out what the kernel reported and nothing else. Analysis
    # runs on the host, so changing how pages are classified costs a rerun of
    # the analysis rather than a rerun of every virtual machine.
    def snapshot(phase):
        d = f"/tmp/proc/{phase}"
        machine.succeed(f"mkdir -p {d}/spl/kstat/zfs {d}/sys/kernel")
        for f in ("iomem", "buddyinfo", "pagetypeinfo", "slabinfo", "meminfo", "vmstat"):
            machine.succeed(f"cat /proc/{f} > {d}/{f}")
        machine.succeed(f"cat /proc/spl/kstat/zfs/arcstats > {d}/spl/kstat/zfs/arcstats")
        for f in ("hostname", "osrelease"):
            machine.succeed(f"cat /proc/sys/kernel/{f} > {d}/sys/kernel/{f}")
        machine.succeed(f"cat /proc/slabwho > {d}/slabwho || true")
        machine.succeed(f"gzip -1 -c /proc/kpageflags > {d}/kpageflags.gz")

    # Read in a seeded shuffle rather than in order: sequential reads let the
    # prefetcher answer most of them and the ARC never holds much at once.
    with subtest("warm"):
        machine.succeed(
            "fragload -mode read -dir /tank/data/set"
            " -files ${toString files} -size ${toString fileSize} -seed ${toString seed}",
            timeout=timedelta(seconds=1800),
        )
        snapshot("warm")

    # A slice stays hot throughout, so that what survives the squeeze is data
    # somebody is holding rather than metadata describing data nobody wants.
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

    # Object mobility only ever acts inside compaction, and nothing above asks
    # for it. Without this the mobility patches would be measured in a state
    # where they are by construction asleep. Every variant gets the same request.
    with subtest("compact"):
        machine.succeed("echo 1 > /proc/sys/vm/compact_memory")
        machine.sleep(duration=timedelta(seconds=10))
        snapshot("compacted")

    os.makedirs(os.environ["out"], exist_ok=True)
    machine.copy_from_machine("/tmp/proc", "")
  '';
}
