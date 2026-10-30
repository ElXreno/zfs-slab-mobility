# How much free memory one compaction pass holds isolated when a large folio finds no target.
{
  pkgs,
  lib,
  variants,
}:

{
  variant,
  seed ? 1,
  memoryMB ? 4096,
  cores ? 2,
  fragMB ? 2048,
  mthpMB ? 1024,
  chunks ? 32,
  maxUnusedPages ? 4096,
}:

let
  name = "compaction-isolate-${variant}-seed${toString seed}";

  py = name: text: pkgs.writeScriptBin name "#!${pkgs.python3.interpreter}\n${text}";

  scene = py "scene" ''
    import mmap
    import sys
    import time

    big, small, chunks = (int(a) for a in sys.argv[1:4])
    big <<= 20
    small <<= 20
    flags = mmap.MAP_PRIVATE | mmap.MAP_ANONYMOUS
    folios = mmap.mmap(-1, big, flags=flags)
    folios.madvise(mmap.MADV_HUGEPAGE)
    pages = mmap.mmap(-1, small, flags=flags)
    pages.madvise(mmap.MADV_NOHUGEPAGE)
    for i in range(chunks):
        for off in range(i * big // chunks, (i + 1) * big // chunks, 4096):
            folios[off] = 1
        for off in range(i * small // chunks, (i + 1) * small // chunks, 4096):
            pages[off] = 1
    for off in range(4096, small, 8192):
        pages.madvise(mmap.MADV_DONTNEED, off, 4096)
    open(sys.argv[4], "w").close()
    time.sleep(1 << 30)
  '';

  freeSampler = py "free-sampler" ''
    import json
    import os
    import sys

    def free():
        with open("/proc/vmstat", "rb") as f:
            for line in f:
                if line.startswith(b"nr_free_pages "):
                    return int(line.split()[1])

    out, ready, stop = sys.argv[1:4]
    base = [free() for _ in range(2000)]
    open(ready, "w").close()
    low = min(base)
    samples = 0
    while not os.path.exists(stop):
        v = free()
        samples += 1
        if v < low:
            low = v
    with open(out, "w") as f:
        json.dump({"base_max": max(base), "base_min": min(base), "low": low, "samples": samples}, f)
  '';
in
(pkgs.testers.runNixOSTest {
  inherit name;

  nodes.machine = {
    virtualisation = {
      memorySize = memoryMB;
      inherit cores;
    };

    boot = {
      kernelPackages = variants.${variant};
      kernelParams = [
        "nokaslr"
        "transparent_hugepage=madvise"
      ];
      kernel.sysctl = {
        "kernel.randomize_va_space" = 0;
        "vm.panic_on_oom" = lib.mkForce 0;
        "vm.compaction_proactiveness" = 0;
      };
    };

    environment.systemPackages = [
      scene
      freeSampler
    ];

    documentation.enable = false;
    services.udisks2.enable = false;
  };

  testScript = ''
    import json
    import os
    import time

    BIN = "/run/current-system/sw/bin"
    THP = "/sys/kernel/mm/transparent_hugepage"
    PAGE = 4096
    MiB = 1048576
    TRACE = "/sys/kernel/tracing"
    EVENT = f"{TRACE}/events/compaction/mm_compaction_isolate_freepages"

    def vmstat():
        return {k: int(v) for k, v in (l.split() for l in machine.succeed("cat /proc/vmstat").splitlines())}

    def mthp_stat(name):
        return int(machine.succeed(f"cat {THP}/hugepages-64kB/stats/{name}").strip())

    machine.start()
    machine.wait_for_unit("multi-user.target")

    machine.succeed(f"echo never > {THP}/hugepages-2048kB/enabled")
    machine.succeed(f"echo madvise > {THP}/hugepages-64kB/enabled")
    machine.succeed("echo 3 > /proc/sys/vm/drop_caches")

    machine.succeed(
        f"systemd-run --unit=scene {BIN}/scene ${toString mthpMB} ${toString fragMB} ${toString chunks} /run/scene-ready"
    )
    machine.wait_until_succeeds("test -e /run/scene-ready", timeout=300)
    folios = mthp_stat("anon_fault_alloc")
    assert folios * 64 * 1024 >= ${toString mthpMB} * MiB * 9 // 10, f"only {folios} 64K folios were faulted in"
    machine.succeed("sync; echo 3 > /proc/sys/vm/drop_caches")
    time.sleep(2)

    machine.succeed(
        f"systemd-run --unit=sampler {BIN}/free-sampler /run/free.json /run/sampler-ready /run/sampler-stop"
    )
    machine.wait_until_succeeds("test -e /run/sampler-ready", timeout=120)
    machine.succeed(f"echo 65536 > {TRACE}/buffer_size_kb && echo > {TRACE}/trace")
    machine.succeed(f"echo 1 > {EVENT}/enable")
    split0 = mthp_stat("split")
    v0 = vmstat()
    started = time.monotonic()
    machine.succeed("echo 1 > /proc/sys/vm/compact_memory", timeout=600)
    took = time.monotonic() - started
    v1 = vmstat()
    split1 = mthp_stat("split")
    machine.succeed(f"echo 0 > {EVENT}/enable")
    machine.succeed("touch /run/sampler-stop")
    machine.wait_until_succeeds("test -e /run/free.json", timeout=120)
    sample = json.loads(machine.succeed("cat /run/free.json"))
    taken = int(machine.succeed(
        f"awk '/mm_compaction_isolate_freepages:/ {{ for (i = 1; i <= NF; i++) if ($i ~ /^nr_taken=/)"
        f" {{ sub(/nr_taken=/, \"\", $i); s += $i }} }} END {{ print s + 0 }}' {TRACE}/trace"
    ).strip())
    events = int(machine.succeed(f"grep -c 'mm_compaction_isolate_freepages:' {TRACE}/trace || true").strip() or 0)
    overrun = int(machine.succeed(f"awk '/^overrun:/ {{ s += $2 }} END {{ print s + 0 }}' {TRACE}/per_cpu/cpu*/stats").strip())

    def d(key):
        return v1.get(key, 0) - v0.get(key, 0)

    dip = max(0, sample["base_max"] - sample["low"])
    unused = taken - d("pgmigrate_success")
    result = {
        "name": "${name}",
        "variant": "${variant}",
        "seed": ${toString seed},
        "free_taken": taken,
        "free_unused": unused,
        "trace_events": events,
        "trace_overrun": overrun,
        "dip_pages": dip,
        "dip_mib": dip * PAGE // MiB,
        "free_before_mib": sample["base_max"] * PAGE // MiB,
        "sampler_samples": sample["samples"],
        "compaction_s": round(took, 3),
        "mthp_faulted": folios,
        "mthp_split": split1 - split0,
        **{k: d(k) for k in (
            "compact_isolated", "compact_migrate_scanned", "compact_free_scanned",
            "pgmigrate_success", "pgmigrate_fail", "compact_stall",
        )},
    }
    print(json.dumps(result))
    out = os.environ["out"]
    os.makedirs(out, exist_ok=True)
    with open(os.path.join(out, "result.json"), "w") as f:
        json.dump(result, f, indent=1)

    assert overrun == 0, f"the trace buffer dropped {overrun} events"
    assert events > 0, "the pass isolated no free pages at all"
    assert unused <= ${toString maxUnusedPages}, (
        f"one compaction pass isolated {taken} free pages and migrated {d('pgmigrate_success')},"
        f" holding {unused} pages ({unused * PAGE // MiB} MiB) it never used out of"
        f" {result['free_before_mib']} MiB free"
    )
  '';
}).overrideTestDerivation
  (_: {
    allowSubstitutes = false;
  })
