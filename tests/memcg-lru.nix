# A stale cache in a young cgroup against the ARC under global reclaim on the multi-gen LRU.
{
  pkgs,
  lib,
  variants,
  fragcheck,
}:

{
  variant,
  seed ? 1,
  memoryMB ? 4096,
  cores ? 2,
  staleMB ? 1024,
  streamFiles ? 4,
  streamFileMB ? 768,
  placeRounds ? 4,
  placeHogMB ? 1536,
  hogMB ? 2048,
  hogSeconds ? 90,
}:

(pkgs.testers.runNixOSTest {
  name = "memcg-lru-${variant}-seed${toString seed}";

  nodes.machine = {
    virtualisation = {
      memorySize = memoryMB;
      inherit cores;
      diskSize = 4096;
      emptyDiskImages = [ 8192 ];
    };

    boot = {
      kernelPackages = variants.${variant};
      supportedFilesystems = [ "zfs" ];
      kernelParams = [ "nokaslr" ];
      kernel.sysctl."vm.panic_on_oom" = lib.mkForce 0;
    };

    networking.hostId = "deadbeef";
    environment.systemPackages = [
      fragcheck
      pkgs.vmtouch
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
    STALE = "/sys/fs/cgroup/stale.slice/stale.service"

    def num(cmd):
        return int(machine.succeed(cmd).strip() or 0)

    def arcstat(name):
        return num(f"awk '$1 == \"{name}\" {{ print $3 }}' /proc/spl/kstat/zfs/arcstats")

    def abdstat(name):
        return num(f"awk '$1 == \"{name}\" {{ print $3 }}' /proc/spl/kstat/zfs/abdstats")

    def stale_file():
        return num(f"awk '$1 == \"file\" {{ print $2 }}' {STALE}/memory.stat")

    def hog(mb, secs, unit):
        machine.succeed(
            f"systemd-run --unit={unit} --collect {BIN}/fragcheck -mode hog -mb {mb} -secs {secs}"
        )

    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.succeed("modprobe zfs")

    enabled = machine.succeed("cat /sys/kernel/mm/lru_gen/enabled").strip()
    assert int(enabled, 16) & 1, f"the multi-gen LRU is off ({enabled})"

    machine.succeed("zpool create -f -o ashift=12 tank /dev/vdb")
    machine.succeed("zfs create -o recordsize=128k -o compression=off -o atime=off tank/data")
    machine.succeed(
        "dd if=/dev/urandom of=/tank/data/stale.bin bs=1M count=${toString staleMB} status=none",
        timeout=timedelta(seconds=600),
    )
    for i in range(${toString streamFiles}):
        machine.succeed(
            f"dd if=/dev/urandom of=/tank/data/stream{i}.bin bs=1M count=${toString streamFileMB} status=none",
            timeout=timedelta(seconds=600),
        )
    machine.succeed("sync; zpool sync tank; echo 3 > /proc/sys/vm/drop_caches")

    # mapped once, as a linker or git maps a file, by a process that stays alive
    machine.succeed(
        "systemd-run --unit=stale --slice=stale.slice -p MemoryMin=infinity"
        f" {BIN}/sh -c '{BIN}/vmtouch -qt /tank/data/stale.bin && {BIN}/touch /run/stale-ready && exec {BIN}/sleep infinity'"
    )
    machine.wait_until_succeeds("test -e /run/stale-ready", timeout=300)
    machine.succeed("systemctl set-property --runtime stale.slice MemoryMin=infinity")
    cached = stale_file()
    assert cached >= ${toString staleMB} * 1048576 * 9 // 10, f"only {cached} bytes of the stale file are in its cgroup"

    # keeps the ARC joining buffers, as the sessions on the machine do
    machine.succeed(
        "systemd-run --unit=reader --collect"
        f" {BIN}/sh -c 'while :; do for f in /tank/data/stream*.bin; do {BIN}/dd if=$f of=/dev/null bs=1M status=none; done; done'"
    )
    machine.wait_until_succeeds(
        "test $(awk '$1 == \"size\" { print $3 }' /proc/spl/kstat/zfs/arcstats) -gt 1073741824",
        timeout=300,
    )

    # below memory.min is trigger 5 in mmzone.h: the cgroup moves to the young generation
    for r in range(${toString placeRounds}):
        hog(${toString placeHogMB}, 10, f"place-hog-{r}")
        machine.wait_until_fails(f"systemctl is-active place-hog-{r}", timeout=300)
    machine.succeed("systemctl set-property --runtime stale.slice MemoryMin=0")
    machine.succeed("systemctl set-property --runtime stale.service MemoryMin=0")
    assert num(f"cat {STALE}/memory.min") == 0
    before = stale_file()

    hog(${toString hogMB}, ${toString hogSeconds}, "anon-hog")
    arc_seen = []
    deadline = time.monotonic() + ${toString hogSeconds} + 600
    while time.monotonic() < deadline:
        arc_seen.append(arcstat("size"))
        if machine.execute("systemctl is-active anon-hog")[0] != 0:
            break
        time.sleep(2)
    status = machine.succeed("systemctl show -p ExecMainStatus --value anon-hog").strip()
    assert status in ("", "0"), f"the hog exited {status}"
    after = stale_file()
    machine.succeed("systemctl stop reader")

    result = {
        "variant": "${variant}",
        "seed": ${toString seed},
        "stale_before": before,
        "stale_after": after,
        "stale_kept_permille": after * 1000 // max(before, 1),
        "arc_min": min(arc_seen),
        "arc_mean": sum(arc_seen) // len(arc_seen),
        "arc_samples": len(arc_seen),
        "lru_bytes_after": abdstat("lru_bytes"),
    }
    print(json.dumps(result))
    os.makedirs(os.environ["out"], exist_ok=True)
    with open(os.path.join(os.environ["out"], "fair.json"), "w") as f:
        json.dump(result, f, indent=1)

    assert result["stale_kept_permille"] <= 500, (
        f"reclaim left {after} of {before} bytes of a cache nobody touched,"
        f" while the ARC went down to {result['arc_min']}"
    )
  '';
}).overrideTestDerivation
  (_: {
    allowSubstitutes = false;
  })
