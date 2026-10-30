# b3sum through mmap over a sparse image, beside idle applications and a warm cache.
{
  pkgs,
  lib,
  variants,
}:

{
  variant,
  fs ? "zfs",
  mmap ? true,
  mglru ? true,
  seed ? 1,
  memoryMB ? 8192,
  cores ? 8,
  sparseGB ? 64,
  strideMB ? 64,
  chunkMB ? 2,
  warmMB ? 1536,
  coldApps ? 3,
  coldMB ? 768,
  swapMB ? 4096,
  streamTimeout ? 3600,
  readAheadKB ? null,
}:

let
  name =
    "mmap-sparse-${fs}-${variant}"
    + lib.optionalString (!mmap) "-nommap"
    + lib.optionalString (!mglru) "-nomglru"
    + lib.optionalString (readAheadKB != null) "-ra${toString readAheadKB}"
    + lib.optionalString (chunkMB == strideMB) "-dense"
    + "-seed${toString seed}";

  setReadAhead = lib.optionalString (readAheadKB != null) (
    lib.concatMapStrings (l: l + "\n") [
      ''machine.succeed("for b in /sys/class/bdi/zfs-*; do echo ${toString readAheadKB} > $b/read_ahead_kb; done")''
      ''ra = machine.succeed("cat /sys/class/bdi/zfs-*/read_ahead_kb").split()''
      ''assert ra and all(r == "${toString readAheadKB}" for r in ra), f"read ahead stayed at {ra}"''
    ]
  );

  chunks = sparseGB * 1024 / strideMB;

  py = b: if b then "True" else "False";

  coldAnon = pkgs.writeScriptBin "cold-anon" ''
    #!${pkgs.python3.interpreter}
    import os
    import sys
    import time

    n = int(sys.argv[1]) << 20
    held = bytearray(n)
    for off in range(0, n, 1 << 20):
        held[off:off + (1 << 20)] = os.urandom(1 << 19).hex().encode()
    open(sys.argv[2], "w").close()
    time.sleep(1 << 30)
  '';
in
(pkgs.testers.runNixOSTest {
  inherit name;

  nodes.machine = {
    virtualisation = {
      memorySize = memoryMB;
      inherit cores;
      diskSize = 4096;
      emptyDiskImages = [
        8192
        swapMB
      ];
    };

    boot = {
      kernelPackages = variants.${variant};
      supportedFilesystems = [ fs ];
      kernelParams = [
        "nokaslr"
        "transparent_hugepage=madvise"
        "zswap.enabled=1"
        "psi=1"
      ];
      kernel.sysctl = {
        "kernel.randomize_va_space" = 0;
        "vm.panic_on_oom" = lib.mkForce 0;
        "vm.swappiness" = 100;
        "vm.vfs_cache_pressure" = 3000;
        "vm.compaction_proactiveness" = 40;
        "vm.min_free_kbytes" = lib.min 262144 (memoryMB * 1024 / 24);
      };
    };

    networking.hostId = "deadbeef";
    environment.systemPackages = [
      coldAnon
      pkgs.b3sum
      pkgs.time
    ];

    documentation.enable = false;
    services.udisks2.enable = false;
  };

  testScript = ''
    import json
    import os
    import time
    from datetime import timedelta

    BIN = "/run/current-system/sw/bin"
    DATA = "/data"
    FS = "${fs}"
    APPS = [f"/sys/fs/cgroup/apps.slice/cold{i}.service" for i in range(${toString coldApps})]
    MiB = 1048576

    def num(cmd):
        return int(machine.succeed(cmd).strip() or 0)

    def vmstat():
        return {k: int(v) for k, v in (l.split() for l in machine.succeed("cat /proc/vmstat").splitlines())}

    def kstat(table, name):
        if FS != "zfs":
            return 0
        return num(f"awk '$1 == \"{name}\" {{ print $3 }}' /proc/spl/kstat/zfs/{table}")

    def meminfo(key):
        return num(f"awk '$1 == \"{key}:\" {{ print $2 * 1024 }}' /proc/meminfo")

    def disk_read():
        return num("awk '{ print $3 * 512 }' /sys/block/vdb/stat")

    def psi_full_us():
        return num("awk '/^full/ { sub(\"total=\", \"\", $5); print $5 }' /proc/pressure/memory")

    def apps_stat(key):
        return sum(num(f"awk '$1 == \"{key}\" {{ print $2 }}' {a}/memory.stat") for a in APPS)

    def apps_swap():
        return sum(num(f"cat {a}/memory.swap.current") for a in APPS)

    def read_warm():
        before = disk_read()
        machine.succeed(f"dd if={DATA}/warm.bin of=/dev/null bs=1M status=none", timeout=timedelta(seconds=600))
        return disk_read() - before

    machine.start()
    machine.wait_for_unit("multi-user.target")

    enabled = machine.succeed("cat /sys/kernel/mm/lru_gen/enabled").strip()
    assert int(enabled, 16) & 1, f"the multi-gen LRU is off ({enabled})"
    if not ${py mglru}:
        machine.succeed("echo n > /sys/kernel/mm/lru_gen/enabled")
        enabled = machine.succeed("cat /sys/kernel/mm/lru_gen/enabled").strip()
        assert int(enabled, 16) == 0, f"the multi-gen LRU stayed on ({enabled})"

    machine.succeed("mkswap /dev/vdc && swapon /dev/vdc")
    machine.succeed("echo 1 > /sys/module/zswap/parameters/enabled")
    machine.succeed("echo Y > /sys/module/zswap/parameters/shrinker_enabled")

    if FS == "zfs":
        machine.succeed("modprobe zfs")
        machine.succeed("zpool create -f -o ashift=12 tank /dev/vdb")
        machine.succeed(
            f"zfs create -o recordsize=128k -o compression=lz4 -o atime=off -o mountpoint={DATA} tank/data"
        )
    else:
        machine.succeed(f"mkfs.xfs -q -f /dev/vdb && mkdir -p {DATA} && mount -o noatime /dev/vdb {DATA}")

    ${setReadAhead}machine.succeed(f"truncate -s ${toString sparseGB}G {DATA}/c.img")
    machine.succeed(
        "for i in $(seq 0 ${toString (chunks - 1)}); do"
        f" dd if=/dev/urandom of={DATA}/c.img bs=1M count=${toString chunkMB}"
        " seek=$((i * ${toString strideMB})) conv=notrunc status=none; done",
        timeout=timedelta(seconds=1200),
    )
    machine.succeed(
        f"dd if=/dev/urandom of={DATA}/warm.bin bs=1M count=${toString warmMB} status=none",
        timeout=timedelta(seconds=600),
    )
    machine.succeed("sync")
    if FS == "zfs":
        machine.succeed("zpool sync tank")
    machine.succeed("echo 3 > /proc/sys/vm/drop_caches")
    allocated = num(f"stat -c '%b %B' {DATA}/c.img | awk '{{ print $1 * $2 }}'")
    expected = ${toString (chunks * chunkMB)} * MiB
    assert expected * 9 // 10 <= allocated <= expected * 3 // 2, f"the image holds {allocated} bytes on disk, wanted about {expected}"

    for _ in range(3):
        read_warm()
    warm_third = read_warm()
    assert warm_third <= ${toString warmMB} * MiB // 10, f"the warm file is not cached: its last read took {warm_third} bytes from disk"

    for i in range(${toString coldApps}):
        machine.succeed(f"systemd-run --unit=cold{i} --slice=apps.slice {BIN}/cold-anon ${toString coldMB} /run/cold{i}-ready")
    for i in range(${toString coldApps}):
        machine.wait_until_succeeds(f"test -e /run/cold{i}-ready", timeout=300)
    cold_before = apps_stat("anon")
    assert cold_before >= ${toString (coldApps * coldMB)} * MiB * 9 // 10, f"the applications hold only {cold_before} bytes"
    assert apps_swap() == 0, "the applications were swapped before the stream"

    v0 = vmstat()
    psi0 = psi_full_us()
    arc_before = kstat("arcstats", "size")
    lru_before = kstat("abdstats", "lru_bytes")

    flags = "--num-threads ${toString cores}" if ${py mmap} else "--no-mmap"
    machine.succeed(
        "systemd-run --unit=stream --slice=stream.slice"
        f" {BIN}/sh -c '{BIN}/time -f \"%e %U %S\" -o /run/stream.time"
        f" {BIN}/b3sum {flags} {DATA}/c.img > /run/stream.out; echo $? > /run/stream.rc'"
    )
    started = time.monotonic()
    mapped_peak = 0
    swap_peak = 0
    arc_min = arc_before
    timed_out = True
    while time.monotonic() - started < ${toString streamTimeout}:
        mapped_peak = max(mapped_peak, meminfo("Mapped"))
        swap_peak = max(swap_peak, apps_swap())
        if FS == "zfs":
            arc_min = min(arc_min, kstat("arcstats", "size"))
        if machine.execute("test -e /run/stream.rc")[0] == 0:
            timed_out = False
            break
        time.sleep(2)
    if timed_out:
        machine.succeed("systemctl stop stream")

    v1 = vmstat()
    psi1 = psi_full_us()
    rc = machine.succeed("cat /run/stream.rc || echo timeout").strip()
    wall, user, sys_ = (float(x) for x in machine.succeed("cat /run/stream.time || echo '0 0 0'").split()[-3:])
    cold_after = apps_stat("anon")
    cold_swap_end = apps_swap()
    arc_after = kstat("arcstats", "size")
    lru_after = kstat("abdstats", "lru_bytes")
    warm_reread = read_warm()
    lru_gen = machine.succeed("cat /sys/kernel/debug/lru_gen 2>/dev/null || true")

    def d(key):
        return v1.get(key, 0) - v0.get(key, 0)

    result = {
        "name": "${name}",
        "fs": FS,
        "variant": "${variant}",
        "mmap": ${py mmap},
        "mglru": ${py mglru},
        "stream_rc": rc,
        "stream_timed_out": timed_out,
        "stream_wall_s": wall,
        "stream_user_s": user,
        "stream_sys_s": sys_,
        "mapped_peak": mapped_peak,
        "cold_before": cold_before,
        "cold_after": cold_after,
        "cold_swap_peak": swap_peak,
        "cold_swap_end": cold_swap_end,
        "cold_swapped_permille": swap_peak * 1000 // max(cold_before, 1),
        "warm_bytes": ${toString warmMB} * MiB,
        "warm_reread_disk": warm_reread,
        "arc_before": arc_before,
        "arc_min": arc_min,
        "arc_after": arc_after,
        "lru_bytes_before": lru_before,
        "lru_bytes_after": lru_after,
        "psi_full_ms": (psi1 - psi0) // 1000,
        **{k: d(k) for k in (
            "pswpout", "pswpin", "zswpout", "zswpin", "zswpwb",
            "pgmajfault", "pgfault", "pgactivate",
            "pgscan_anon", "pgscan_file", "pgsteal_anon", "pgsteal_file",
            "pgscan_kswapd", "pgscan_direct", "allocstall_normal", "allocstall_movable",
            "workingset_refault_anon", "workingset_refault_file",
        )},
    }
    print(json.dumps(result))
    out = os.environ["out"]
    os.makedirs(out, exist_ok=True)
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(result, f, indent=1)
    with open(os.path.join(out, "lru_gen.txt"), "w") as f:
        f.write(lru_gen)

    assert not timed_out, "the stream did not finish in ${toString streamTimeout} s"
    assert rc == "0", f"b3sum exited {rc}"
  '';
}).overrideTestDerivation
  (_: {
    allowSubstitutes = false;
  })
