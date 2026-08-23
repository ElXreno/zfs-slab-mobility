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
      modulePageMobility ? false,
      noReclaimAccount ? false,
      slabMobilityZfs ? false,
      withProbes ? false,
      noKswapdWake ? false,
      memProfiling ? false,
      kasan ? false,
    }:
    let
      extraPatches =
        lib.optional slabMobility (kernelPatch "slab-object-mobility")
        ++ lib.optional modulePageMobility (kernelPatch "module-movable-pages");

      # Only wrap the compiler where a kernel is actually built. Wrapping it
      # unconditionally changes the derivation for the unpatched variants too,
      # and those would stop coming out of the binary cache.
      kernel =
        if extraPatches == [ ] && !memProfiling && !kasan then
          pkgs.linux_latest
        else
          pkgs.linux_latest.override {
            stdenv = ccache.wrapStdenv pkgs.stdenv;
            kernelPatches = pkgs.linux_latest.kernelPatches ++ extraPatches;
            # KASAN makes the kernel's Rust support unavailable, and nixpkgs
            # asks for it in the config it shares with every kernel, so the
            # strict check reports options that were never ours. Loosened for
            # this variant alone; everything measured keeps the check.
            ignoreConfigErrors = kasan;
            structuredExtraConfig =
              lib.optionalAttrs memProfiling {
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
          };

      zfsExtra =
        (
          if slabMobilityZfs then
            (if withProbes then patches.zfs.withProbes else patches.zfs.relocation)
          else
            lib.optional noReclaimAccount patches.zfs.each.no-reclaim-account
        )
        ++ lib.optional noKswapdWake patches.zfs.each.no-kswapd-wake;

      packages = pkgs.linuxPackagesFor kernel;
    in
    packages.extend (
      final: prev: {
        slabwho = final.callPackage ../packages/slabwho/package.nix { };

        zfs_2_4 = prev.zfs_2_4.overrideAttrs (old: {
          patches = (old.patches or [ ]) ++ zfsExtra;

          # OpenZFS refuses at configure time to build against a kernel newer
          # than the one it was tested on, and 2.4.3 stops at 7.0, which is
          # already end of life and gone from nixpkgs. The behaviour under study
          # needs a kernel with per CPU sheaves in SLUB, so the ceiling is lifted
          # deliberately rather than the kernel moved back below the feature.
          postPatch = (old.postPatch or "") + ''
            substituteInPlace META --replace-fail "Linux-Maximum: 7.0" "Linux-Maximum: 7.1"
          '';
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

  # The above plus object relocation in SLUB and movable pages for the scatter
  # ABD, so that what is left in a block can be moved out of the way. Carries
  # what a machine would run and nothing else, because everything it is
  # compared against runs a stock allocator: a probe that changes how a cache
  # is built would be a difference of its own, counted as if it were this one.
  mobility = mkVariant {
    slabMobility = true;
    modulePageMobility = true;
    slabMobilityZfs = true;
  };

  # Relocation under KASAN. A bad-page report says only that the allocator
  # found damage; KASAN says who freed the object and who touched it after.
  # Slow enough that it is a variant of its own rather than something the
  # measured runs carry.
  kasan = mkVariant {
    slabMobility = true;
    modulePageMobility = true;
    slabMobilityZfs = true;
    kasan = true;
  };

  # The same, plus the probes. Kept apart because dbuf-move-probe creates its
  # cache mobile, and that flag costs the cache its per CPU sheaves and its
  # merging, which moves the slab footprint the comparisons above measure.
  probes = mkVariant {
    slabMobility = true;
    modulePageMobility = true;
    slabMobilityZfs = true;
    withProbes = true;
  };
}
