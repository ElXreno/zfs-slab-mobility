# zfs-slab-mobility

Why a machine running ZFS ends up with plenty of free memory and none of it in
large enough pieces, and what to do about it.

## The short version

ZFS marks both halves of its memory as reclaimable: the slab caches through
`KMC_RECLAIMABLE` becoming `SLAB_RECLAIM_ACCOUNT`, and the ABD data pages
through `__GFP_RECLAIMABLE` in `abd_alloc_chunks()`. The buddy allocator files
them into the same pageblocks.

The two do not mean the same thing by "reclaimable". An ABD chunk really is
handed back on request. A slab page is only freed once its last object dies,
and for `arc_buf_hdr_t_full`, `dmu_buf_impl_t` and `dnode_t` that is rare. So a
block that held both never comes free: the data goes back, a handful of live
objects stay, and the block is out of reach for anything wanting a high order
allocation.

On a 64 GB laptop after dropping caches, that came to 8732 pageblocks holding
16.4 GiB of free memory hostage, with the same amount of memory free as on a
freshly booted machine.

## What is here

- `patches/zfs/no-reclaim-account.patch` removes one line, so the slab caches
  stop asking for the pageblocks the data pages live in.
- `patches/kernel/` and the rest of `patches/zfs/` add object relocation to
  SLUB and make scatter ABD pages movable, so that what is left in a block can
  be moved out of the way.
- `packages/fragview` reads `/proc/kpageflags` and shows what every pageblock
  is made of, live or from a snapshot. A test guest copies the kernel's own
  reporting out verbatim and this reads it back with `-proc-root`, so changing
  how pages are classified costs a rerun of the analysis rather than a rerun of
  every virtual machine.
- `packages/fragload` writes and reads a file set that is identical on every
  run for a given seed.
- `tests/` boots a VM per variant and asserts on the difference between them.

## Measured

Median of three seeds, after dropping caches and compacting, in a six gigabyte
guest of 3584 pageblocks. These are the numbers `nix flake check` produces, so
they can be reproduced rather than taken on trust.

| | stock | separation | mobility |
|---|---|---|---|
| mixed blocks | 510 | 297 | 274 |
| hostage blocks | 466 | 181 | 138 |
| pinned | 884 MiB | 342 MiB | 259 MiB |
| slab pages inside them | 6343 | 389 | 1260 |
| unusable index at order 10 | 35% | 19% | 16% |
| Movable / Reclaimable blocks | 1278 / 1483 | 1322 / 1429 | 2714 / 13 |

Expect these to move a few per cent between runs. Two full runs of the suite
put the ratio of mixed blocks between stock and separation at 0.559 and 0.582,
and the pinned memory at 0.346 and 0.387, against a threshold of 0.8.

The ARC holds the same amount in all three, so the memory is not going
anywhere different, only being grouped differently.

Separation does most of the work here: not letting the slab caches share
pageblocks with the data pages takes the pinned memory to a third and the slab
pages left inside nearly empty blocks from 6853 to 412.

Mobility then shows up in the migrate types, where the transfer is almost exact:
Movable gains 1401 blocks and Reclaimable loses 1397. With the slab out of the
reclaimable pool and the scatter ABD asking for movable pages, ZFS has nothing
left in Reclaimable at all, and the fourteen blocks remaining are the rest of
the system.

Compaction without mobility makes things worse: it consolidates the movable
pages and leaves blocks that still hold one immovable page more exposed, so the
hostage count goes up 16 to 19 per cent. With mobility it does not move.

`vm.defrag_mode=1` makes no measurable difference on a stock kernel: the pinned
memory comes out within five per cent either way, against a spread of eight per
cent between seeds of the same build. It tells the allocator to compact rather
than mix migrate types, which is worth something only if the pages it wants to
compact can move.

## Running it

```console
$ nix build -j1 -L .#checks.x86_64-linux.separation
$ cat result/summary.txt
```

`-j1` is not optional. A comparison is six guests of six gigabytes each, and
nix will happily start several at once, which is more memory than the machine
running them is likely to have.

A single pass, for looking at rather than asserting on. It lives under
`packages` because it asserts nothing, and because CI gives every check its own
runner and these are runs the comparisons already build:

```console
$ nix build -j1 -L .#run-mobility
$ nix run .#fragview -- -proc-root result/proc/dropped -1
```

A comparison keeps the snapshots it made, so they can be opened directly:

```console
$ nix run .#fragview -- -load result/mobility-1.snap
```

`fragview` draws a pixel map under kitty and falls back to characters
elsewhere. Hovering a block names what is in it.

## Object cache

Only the mobility variant compiles a kernel; the other two take stock
`linux_latest` from the binary cache. That one build is wrapped in ccache,
which does nothing until `/nix/ccache` is reachable from the sandbox:

```nix
nix.settings.extra-sandbox-paths = [ "/nix/ccache" ];
systemd.tmpfiles.rules = [ "d /nix/ccache 0777 root root -" ];
```

A build here never evicts anything: it runs with no size limit, so trimming is
left to whoever owns the directory. Saying nothing instead would be worse than
setting a limit, because the sandbox carries only the derivation's closure and a
`ccache.conf` placed as a store symlink dangles inside it. ccache then falls
back to its five gigabyte default and starts evicting a cache many times that
size on every compile.

The path is a build time constant so that a kernel built in CI matches one
built locally. To back it with a directory you already have, map it rather than
changing it:

```nix
nix.settings.extra-sandbox-paths = [ "/nix/ccache=/var/cache/ccache" ];
```

`extra-sandbox-paths` is a restricted setting, so passing it as `--option` on
the command line is silently ignored unless you are in `trusted-users`.

## On the kernel exports

The mobility patches need three symbols reachable from a module that is not
GPL: `set_movable_ops` is changed from `EXPORT_SYMBOL_GPL` to `EXPORT_SYMBOL`,
and `kmem_cache_setup_mobility` and `kmem_cache_defrag` are exported the same
way. OpenZFS declares `MODULE_LICENSE("CDDL")`, so a GPL-only export is not
reachable from it at all.

This is worth being plain about. A patch that widens an export so that a CDDL
module can use it is not something the kernel community entertains, and it will
not be proposed upstream. Treat the mobility half of this repository as an
experiment in what object relocation would buy, not as a patch series looking
for a merge.

The separation patch has none of this. It removes one line inside OpenZFS,
touches no kernel symbol and needs no kernel patch at all, which is why it is
the half worth taking seriously.

Nothing here distributes a built kernel: CI publishes the comparison tables,
the metrics and the snapshots, and keeps its build cache inside the GitHub
Actions cache.

## Caveats

- OpenZFS 2.4.3 refuses at configure time to build against a kernel newer than
  7.0, which is already end of life and gone from nixpkgs. The behaviour under
  study needs a kernel with per CPU sheaves in SLUB, so `nix/variants.nix`
  lifts that ceiling deliberately.
- `slabwho`, the module that answers which cache owns a slab page, mirrors
  `struct kmem_cache` and therefore tracks one kernel version at a time. The
  block columns simply stay empty without it.
- The absolute numbers depend on the guest size and the workload. Only the
  ratio between two builds measured back to back means anything, which is why
  every check compares builds rather than testing one against a constant.
