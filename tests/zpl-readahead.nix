# Readahead into the page cache of ZFS files: a hang against writers, and the bytes it fills.
{
  pkgs,
  lib,
  variants,
  mmapcheck,
}:

{
  variant,
  scene,
  seed ? 1,
  readAheadKB ? null,
  memoryMB ? 4096,
  cores ? 4,
  seconds ? 120,
  stall ? 20,
  rounds ? 10,
  holdMs ? 1000,
  trace ? false,
  expectWrong ? false,
  expectStall ? false,
  inject ? false,
}:

let
  name =
    "zpl-readahead-${scene}-${variant}"
    + lib.optionalString inject "-inject"
    + lib.optionalString (readAheadKB != null) "-ra${toString readAheadKB}"
    + "-seed${toString seed}";

  py = b: if b then "True" else "False";

  injectArg = lib.optionalString inject " -inject 'zinject -d vdb -D ${toString holdMs}:1 -T read tank'";
in
(pkgs.testers.runNixOSTest {
  inherit name;

  nodes.machine = {
    virtualisation = {
      memorySize = memoryMB;
      inherit cores;
      emptyDiskImages = [ 4096 ];
    };

    boot = {
      kernelPackages = variants.${variant};
      supportedFilesystems = [ "zfs" ];
      kernelParams = [ "nokaslr" ];
      kernel.sysctl."vm.panic_on_oom" = lib.mkForce 0;
    };

    networking.hostId = "deadbeef";
    environment.systemPackages = [ mmapcheck ];

    documentation.enable = false;
    services.udisks2.enable = false;
  };

  testScript = ''
    import json
    import os

    BIN = "/run/current-system/sw/bin"
    out = os.environ["out"]
    os.makedirs(out, exist_ok=True)

    def save(result):
        print(json.dumps(result))
        with open(os.path.join(out, "result.json"), "w") as f:
            json.dump(result, f, indent=1)

    def set_readahead():
        if not ${py (readAheadKB != null)}:
            return
        machine.succeed("for b in /sys/class/bdi/zfs-*; do echo ${toString readAheadKB} > $b/read_ahead_kb; done")
        ra = machine.succeed("cat /sys/class/bdi/zfs-*/read_ahead_kb").split()
        assert ra and all(r == "${toString readAheadKB}" for r in ra), f"read ahead stayed at {ra}"

    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.succeed("modprobe zfs && zpool create -f -o ashift=12 tank /dev/vdb")

    if "${scene}" == "deadlock":
        machine.succeed("zfs create -o recordsize=4k -o compression=on -o atime=off tank/fs")
        set_readahead()
        machine.succeed(
            f"systemd-run --unit=ra {BIN}/mmapcheck -seconds ${toString seconds} -stall ${toString stall}"
            " -result /run/ra.result deadlock /tank/fs/file"
        )
        machine.wait_until_succeeds(
            "grep -qE '^(OK|STALLED|ERROR)' /run/ra.result", timeout=${toString (seconds + 300)}
        )
        lines = machine.succeed("cat /run/ra.result").splitlines()
        verdict = next(l for l in lines if l.split()[0] in ("OK", "STALLED", "ERROR"))
        stacks = ""
        if not verdict.startswith("OK"):
            stacks = machine.succeed(
                "for t in /proc/[0-9]*/task/[0-9]*; do"
                " grep -q '^State:.*D (disk sleep)' $t/status 2>/dev/null || continue;"
                " echo \"== $t $(cat $t/comm)\"; cat $t/stack; done"
            )
            print(stacks)
            with open(os.path.join(out, "stacks.txt"), "w") as f:
                f.write(stacks)
        progress = [l for l in lines if l.endswith(")")]
        save({
            "name": "${name}",
            "variant": "${variant}",
            "scene": "deadlock",
            "read_ahead_kb": ${if readAheadKB == null then "None" else toString readAheadKB},
            "verdict": verdict,
            "last_progress": progress[-1] if progress else "",
            "blocked_tasks": stacks.count("== /proc/"),
            "expect_stall": ${py expectStall},
        })
        if stacks:
            machine.crash()
        if ${py expectStall}:
            assert verdict.startswith("STALLED"), f"readahead against writers did not hang: {verdict}"
        else:
            assert verdict.startswith("OK"), f"readahead against writers: {verdict}"

    elif "${scene}" == "subpage":
        machine.succeed("zfs create -o recordsize=128k -o compression=off -o atime=off tank/fs")
        if ${py trace}:
            machine.succeed(
                "cd /sys/kernel/tracing"
                " && echo 'p:zra zpl_readahead' >> kprobe_events"
                " && echo 'r:ztry zfs_rangelock_tryenter ret=$retval' >> kprobe_events"
                " && (echo 'p:zup update_pages' >> kprobe_events || true)"
                " && (echo 'p:zfp zfs_fillpage' >> kprobe_events || true)"
                " && echo 1 > events/kprobes/enable"
                " && echo 1 > events/filemap/mm_filemap_add_to_page_cache/enable"
                " && echo 1 > events/filemap/mm_filemap_delete_from_page_cache/enable"
            )
        runs = []
        for i in range(${toString rounds}):
            machine.succeed(
                "head -c 262144 /dev/urandom > /run/orig.bin && cp /run/orig.bin /tank/fs/file"
                " && sync && zpool sync tank && zpool export tank && zpool import -d /dev tank"
            )
            set_readahead()
            if ${py trace}:
                machine.succeed("echo > /sys/kernel/tracing/trace")
            line = machine.succeed(
                f"{BIN}/mmapcheck -orig /run/orig.bin -hold ${toString holdMs}"
                " -inject 'zinject -d vdb -D ${toString holdMs}:1 -T read tank' subpage /tank/fs/file",
                timeout=120,
            )
            runs.append(json.loads(line.strip().splitlines()[-1]))
            if ${py trace}:
                ino = machine.succeed("stat -c %i /tank/fs/file").strip()
                trace = machine.succeed("cat /sys/kernel/tracing/trace")
                print(f"TRACE round {i} ino {ino}\n{trace}")
                with open(os.path.join(out, f"trace-{i}.txt"), "w") as f:
                    f.write(f"ino {ino} {int(ino):x}\n" + trace)
        held = [
            r for r in runs
            if r["cached"] and r["writer_ms"] >= ${toString (holdMs * 8 / 10)} and r["fadvise_done_ms"] < r["writer_ms"]
        ]
        wrong = [r for r in runs if r["pread_wrong"] or r["mmap_wrong"]]
        save({
            "name": "${name}",
            "variant": "${variant}",
            "scene": "subpage",
            "read_ahead_kb": ${if readAheadKB == null then "None" else toString readAheadKB},
            "rounds": len(runs),
            "held": len(held),
            "wrong": len(wrong),
            "expect_wrong": ${py expectWrong},
            "runs": runs,
        })
        assert len(held) * 2 >= len(runs), f"the write was held across WILLNEED in only {len(held)} of {len(runs)} rounds"
        if ${py expectWrong}:
            assert len(wrong) * 2 >= len(held), f"only {len(wrong)} of {len(held)} held rounds read back other bytes"
        else:
            assert not wrong, f"{len(wrong)} of {len(runs)} rounds read back other bytes: {wrong[0]}"

    elif "${scene}" == "zeropage":
        machine.succeed("zfs create -o recordsize=128k -o compression=off -o atime=off -o primarycache=metadata tank/fs")
        runs = []
        for _ in range(${toString rounds}):
            machine.succeed(
                "head -c 8292 /dev/urandom > /run/orig.bin && cp /run/orig.bin /tank/fs/file"
                " && sync && zpool sync tank && zpool export tank && zpool import -d /dev tank"
            )
            set_readahead()
            blksz = int(machine.succeed("stat -c %o /tank/fs/file").strip())
            line = machine.succeed(
                f"{BIN}/mmapcheck -orig /run/orig.bin -hold ${toString holdMs}${injectArg} zeropage /tank/fs/file",
                timeout=120,
            )
            res = json.loads(line.strip().splitlines()[-1])
            res["blksz"] = blksz
            runs.append(res)
        if ${py inject}:
            held = [r for r in runs if r["cached"] and r["writer_ms"] >= ${toString (holdMs * 8 / 10)}]
        else:
            held = [r for r in runs if r["cached"]]
        wrong = [r for r in runs if r["pread_wrong"] or r["mmap_wrong"]]
        save({
            "name": "${name}",
            "variant": "${variant}",
            "scene": "zeropage",
            "inject": ${py inject},
            "read_ahead_kb": ${if readAheadKB == null then "None" else toString readAheadKB},
            "rounds": len(runs),
            "held": len(held),
            "wrong": len(wrong),
            "expect_wrong": ${py expectWrong},
            "runs": runs,
        })
        assert len(held) * 2 >= len(runs), f"the precondition held in only {len(held)} of {len(runs)} rounds"
        if ${py expectWrong}:
            assert len(wrong) * 2 >= len(held), f"only {len(wrong)} of {len(held)} held rounds read back other bytes"
        else:
            assert not wrong, f"{len(wrong)} of {len(runs)} rounds read back other bytes: {wrong[0]}"

    else:
        datasets = {
            "rs512": "-o recordsize=512 -o compression=off",
            "rs4k": "-o recordsize=4k -o compression=lz4",
            "rs128k": "-o recordsize=128k -o compression=lz4",
            "rs1m": "-o recordsize=1M -o compression=off",
        }
        files = []
        for ds, props in datasets.items():
            d = f"/tank/{ds}"
            machine.succeed(f"zfs create {props} -o atime=off tank/{ds}")
            machine.succeed(
                f"head -c $((37 * 1048576 + 123)) /dev/urandom > {d}/big.bin"
                f" && head -c $((8 * 1048576)) /dev/zero > {d}/zeros.bin"
                f" && truncate -s 64M {d}/sparse.bin"
                f" && for o in 0 $((7 * 1048576 + 4095)) $((33 * 1048576)) $((63 * 1048576 + 1)); do"
                f" head -c 300001 /dev/urandom | dd of={d}/sparse.bin bs=65536 seek=$o oflag=seek_bytes"
                " conv=notrunc status=none; done"
                f" && : > {d}/empty.bin"
                f" && for n in 1 3 4096 4097 5000 8191 131073; do head -c $n /dev/urandom > {d}/small-$n.bin; done"
            )
            files += [f"{d}/{n}" for n in ("big.bin", "zeros.bin", "sparse.bin", "empty.bin")]
            files += [f"{d}/small-{n}.bin" for n in (1, 3, 4096, 4097, 5000, 8191, 131073)]
        machine.succeed("sync && zpool sync tank && echo 3 > /proc/sys/vm/drop_caches")
        set_readahead()
        paths = " ".join(files)
        mapped = {}
        faults = {}
        for line in machine.succeed(f"{BIN}/mmapcheck sum {paths}").splitlines():
            digest, path, majflt = line.split()
            mapped[path] = digest
            faults[path] = int(majflt)
        read = {}
        for line in machine.succeed(f"sha256sum {paths}").splitlines():
            digest, path = line.split()
            read[path] = digest
        wrong = sorted(p for p in files if mapped.get(p) != read.get(p))
        save({
            "name": "${name}",
            "variant": "${variant}",
            "scene": "bytes",
            "read_ahead_kb": ${if readAheadKB == null then "None" else toString readAheadKB},
            "files": len(files),
            "wrong": wrong,
            "big_majflt": {p: faults[p] for p in files if p.endswith("/big.bin")},
            "big_pages": (37 * 1048576 + 123 + 4095) // 4096,
        })
        assert not wrong, f"the mapping read other bytes than read() in {wrong}"
  '';
}).overrideTestDerivation
  (_: {
    allowSubstitutes = false;
  })
