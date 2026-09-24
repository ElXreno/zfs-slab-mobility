# The three builds under test, as a plain data description.
#
# Each variant is the same vanilla kernel and the same OpenZFS release with a
# different set of patches on top, so a difference between two runs can only
# come from the patches. Nothing here is specific to any machine: the kernel is
# whatever nixpkgs calls latest, with no microarchitecture flags.
{ pkgs, lib }:

let
  ccache = import ./ccache.nix { inherit pkgs; };

  # The series itself lives next door, because a machine that runs what this
  # measures has to apply the same files in the same order.
  patches = import ./patches.nix;

  kernelPatch = name: {
    inherit name;
    patch = patches.kernel.${name};
  };

  mkVariant =
    {
      slabMobility ? false,
      filemapExports ? false,
      largeFolioCompaction ? false,
      noReclaimAccount ? false,
      slabMobilityZfs ? false,
      arcLru ? false,
      withProbes ? false,
      noKswapdWake ? false,
      deadlockInject ? false,
      memProfiling ? false,
      kasan ? false,
      zfsDebug ? false,
      # The kernel the hung desktop runs, working set protection included.
      xanmod ? false,
    }:
    let
      extraPatches =
        lib.optional slabMobility (kernelPatch "slab-object-mobility")
        ++ lib.optional filemapExports (kernelPatch "filemap-exports")
        ++ lib.optional largeFolioCompaction (kernelPatch "compaction-large-folio");

      # Boot-time shuffling of object and page placement, none of it switchable at runtime.
      deterministic = {
        SLAB_FREELIST_RANDOM = lib.mkForce lib.kernel.no;
        KMALLOC_PARTITION_CACHES = lib.mkForce lib.kernel.no;
        KMALLOC_PARTITION_RANDOM = lib.mkForce lib.kernel.unset;
        SHUFFLE_PAGE_ALLOCATOR = lib.mkForce lib.kernel.no;
      };

      base = if xanmod then pkgs.linux_xanmod_latest else pkgs.linux_latest;

      standConfig =
          deterministic
          // lib.optionalAttrs memProfiling {
                MEM_ALLOC_PROFILING = lib.kernel.yes;
                MEM_ALLOC_PROFILING_ENABLED_BY_DEFAULT = lib.kernel.yes;
                MEM_ALLOC_PROFILING_DEBUG = lib.kernel.no;
              }
          // lib.optionalAttrs kasan {
            # Names both ends of a use after free instead of leaving the
            # allocator to notice the damage later, which is all a bare
            # bad-page report can say.
            KASAN = lib.kernel.yes;
            KASAN_GENERIC = lib.kernel.yes;
            KASAN_OUTLINE = lib.kernel.yes;
            # Folio and list invariants checked where KASAN alone sees only bytes.
            DEBUG_VM = lib.kernel.yes;
            DEBUG_LIST = lib.kernel.yes;
            # Poisons a type safe object after its grace period, which the
            # relocation callback may still read by contract; see the memory note.
            SLUB_RCU_DEBUG = lib.kernel.no;
            # ZFS refuses to build against a kernel carrying lockdep:
            # mutex_lock becomes GPL only and configure gives up. Nothing
            # here selects it, but olddefconfig is happy to turn it back
            # on, so it is spelled out.
            # LOCKDEP itself is selected, never set: naming it here is an
            # error rather than a no-op. These are the ones that select it.
            PROVE_LOCKING = lib.kernel.no;
            DEBUG_LOCK_ALLOC = lib.kernel.no;
            DEBUG_MUTEXES = lib.kernel.no;
            DEBUG_RWSEMS = lib.kernel.no;
            DEBUG_SPINLOCK = lib.kernel.no;
          };

      # xanmod's own config wins over an overridden structuredExtraConfig, so
      # there the stand's settings ride along as a patch without a diff.
      kernel = base.override (
        {
          stdenv = ccache.wrapStdenv pkgs.stdenv;
          kernelPatches =
            base.kernelPatches
            ++ extraPatches
            ++ lib.optional xanmod {
              name = "stand-config";
              patch = null;
              structuredExtraConfig = standConfig;
            };
          # KASAN removes the Rust support the shared nixpkgs config asks for.
          ignoreConfigErrors = kasan;
        }
        // lib.optionalAttrs (!xanmod) { structuredExtraConfig = standConfig; }
      );

      zfsExtra =
        (
          if arcLru then
            (if withProbes then patches.zfs.arclruWithProbes else patches.zfs.arclru)
          else if slabMobilityZfs then
            (if withProbes then patches.zfs.withProbes else patches.zfs.relocation)
          else
            lib.optional noReclaimAccount patches.zfs.each.no-reclaim-account
        )
        ++ lib.optional noKswapdWake patches.zfs.each.no-kswapd-wake
        ++ lib.optional deadlockInject patches.zfs.each.arc-lru-deadlock-inject;

      packages = pkgs.linuxPackagesFor kernel;
    in
    packages.extend (
      final: prev: {
        slabwho = final.callPackage ../packages/slabwho/package.nix { };

        zfs_2_4 = prev.zfs_2_4.overrideAttrs (old: {
          patches = (old.patches or [ ]) ++ zfsExtra;
          # ASSERTs on, so the read only invariant of joined buffers is checked.
          configureFlags = (old.configureFlags or [ ]) ++ lib.optional zfsDebug "--enable-debug";
        });
      }
    );
in
{
  # Vanilla everything. ZFS marks its slab caches and its data pages alike as
  # reclaimable, so the allocator files them into the same pageblocks.
  stock = mkVariant { };

  # One line removed from spl_kmem_cache_create, so that the slab caches stop
  # asking for the pageblocks the data pages live in.
  separation = mkVariant { noReclaimAccount = true; };

  # Separation plus one line that stops a cache growing through vmalloc from
  # waking kswapd, which on this module runs the arc shrinker and frees what the
  # cache was being grown to hold.
  nokswapd = mkVariant {
    noReclaimAccount = true;
    noKswapdWake = true;
  };

  # Allocation profiling, which gives every slab object a codetag naming the
  # line that allocated it. SLUB merges caches of the same size and keeps one
  # name for all of them, so this is the only way to tell what a block is
  # really held by. Its own cost is a pointer per object, so it is a separate
  # variant rather than something the measured ones carry.
  profiling = mkVariant { memProfiling = true; };

  # Header relocation plus clean ARC buffers as folios in the page cache, which
  # the kernel reclaims and moves itself, so the abd-* series is not applied.
  arclru = mkVariant {
    slabMobility = true;
    filemapExports = true;
    largeFolioCompaction = true;
    arcLru = true;
  };

  # Relocation under KASAN. A bad-page report says only that the allocator
  # found damage; KASAN says who freed the object and who touched it after.
  # Slow enough that it is a variant of its own rather than something the
  # measured runs carry.
  kasan = mkVariant {
    slabMobility = true;
    filemapExports = true;
    largeFolioCompaction = true;
    arcLru = true;
    kasan = true;
    zfsDebug = true;
  };

  # The backend plus the knob that makes a reclaimer meet a chunk of the other
  # buffer already locked. Debug build: the knob exists only there.
  inject = mkVariant {
    slabMobility = true;
    filemapExports = true;
    largeFolioCompaction = true;
    arcLru = true;
    deadlockInject = true;
    zfsDebug = true;
  };

  # The same, plus the probes. Kept apart because dbuf-move-probe creates its
  # cache mobile, and that flag costs the cache its per CPU sheaves and its
  # merging, which moves the slab footprint the comparisons above measure.
  probes = mkVariant {
    slabMobility = true;
    filemapExports = true;
    largeFolioCompaction = true;
    arcLru = true;
    withProbes = true;
  };

  # The backend and stock on the kernel of the machine that hung.
  arclruXanmod = mkVariant {
    slabMobility = true;
    filemapExports = true;
    largeFolioCompaction = true;
    arcLru = true;
    xanmod = true;
  };

  stockXanmod = mkVariant { xanmod = true; };
}
