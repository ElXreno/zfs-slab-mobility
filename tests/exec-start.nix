# A cold compiler start with its libraries mapped from ZFS, against the readahead window.
{
  pkgs,
  variants,
}:

{
  variant,
  readAheadKB ? null,
  seed ? 1,
  rounds ? 7,
  memoryMB ? 4096,
  cores ? 4,
}:

let
  ra = if readAheadKB == null then "default" else toString readAheadKB;

  name = "exec-start-${variant}-ra${ra}-seed${toString seed}";

  clang = pkgs.llvmPackages.clang-unwrapped;
in
(pkgs.testers.runNixOSTest {
  inherit name;

  nodes.machine = {
    virtualisation = {
      memorySize = memoryMB;
      inherit cores;
      emptyDiskImages = [
        {
          size = 4096;
          driveConfig.driveExtraOpts = {
            cache = "none";
            aio = "native";
          };
        }
      ];
    };

    boot = {
      kernelPackages = variants.${variant};
      supportedFilesystems = [ "zfs" ];
      kernelParams = [ "nokaslr" ];
    };

    networking.hostId = "deadbeef";
    environment.systemPackages = [ pkgs.time ];

    documentation.enable = false;
    services.udisks2.enable = false;
  };

  testScript = ''
    import json
    import os
    import statistics

    BIN = "/run/current-system/sw/bin"
    CC = "/tank/cc"
    MiB = 1048576

    def disk_read():
        return int(machine.succeed("awk '{ print $3 * 512 }' /sys/block/vdb/stat").strip())

    SET: str = "${if readAheadKB == null then "" else toString readAheadKB}"

    def cold():
        machine.succeed("zpool export tank && zpool import -d /dev tank")
        if SET:
            machine.succeed(f"for b in /sys/class/bdi/zfs-*; do echo {SET} > $b/read_ahead_kb; done")
        ra = set(machine.succeed("cat /sys/class/bdi/zfs-*/read_ahead_kb").split())
        assert len(ra) == 1 and (not SET or ra == {SET}), f"read ahead is {ra}"
        machine.succeed("sync && echo 3 > /proc/sys/vm/drop_caches")
        return int(ra.pop())

    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.succeed("modprobe zfs && zpool create -f -o ashift=12 tank /dev/vdb")
    machine.succeed("zfs create -o recordsize=128k -o compression=lz4 -o atime=off -o mountpoint=/tank/cc tank/cc")
    machine.succeed(
        f"mkdir -p {CC}/lib && cp -L ${clang}/bin/clang {CC}/clang"
        f" && ldd ${clang}/bin/clang | awk '/=> \\// {{ print $3 }} /^\\s*\\// {{ print $1 }}'"
        f" | xargs -I{{}} cp -L {{}} {CC}/lib/"
    )
    loader = machine.succeed(f"ls {CC}/lib/ld-linux-x86-64.so.2").strip()
    machine.succeed("echo 'int f(int x) { return x * 2; }' > /run/t.c")
    run = (
        f"{BIN}/time -f '%e %F %R' -o /run/t.time {loader} --library-path {CC}/lib"
        f" {CC}/clang -fintegrated-cc1 -ffreestanding -nostdinc -O2 -c /run/t.c -o /run/t.o"
    )
    machine.succeed(run)
    size = int(machine.succeed(f"du -sb --apparent-size {CC} | cut -f1").strip())

    walls = []
    disk = []
    majflt = []
    window = 0
    for _ in range(${toString rounds}):
        window = cold()
        before = disk_read()
        machine.succeed(run)
        disk.append(disk_read() - before)
        wall, major, _minor = machine.succeed("cat /run/t.time").split()
        walls.append(float(wall))
        majflt.append(int(major))

    result = {
        "name": "${name}",
        "variant": "${variant}",
        "read_ahead_kb": window,
        "payload_mib": size // MiB,
        "walls": walls,
        "median_wall": statistics.median(walls),
        "disk_mib": [d / MiB for d in disk],
        "median_disk_mib": statistics.median(disk) / MiB,
        "majflt": majflt,
        "median_majflt": statistics.median(majflt),
    }
    print(json.dumps(result))
    out = os.environ["out"]
    os.makedirs(out, exist_ok=True)
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(result, f, indent=1)
  '';
}).overrideTestDerivation
  (_: {
    allowSubstitutes = false;
  })
