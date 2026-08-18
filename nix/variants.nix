# The three builds under test, as a plain data description.
#
# Each variant is the same vanilla kernel and the same OpenZFS release with a
# different set of patches on top, so a difference between two runs can only
# come from the patches. Nothing here is specific to any machine: the kernel is
# whatever nixpkgs calls latest, with no microarchitecture flags.
{ pkgs, lib }:

let
  ccache = import ./ccache.nix { inherit pkgs; };

  kernelPatch = name: {
    inherit name;
    patch = ../patches/kernel/${name}.patch;
  };

  zfsPatch = name: ../patches/zfs/${name}.patch;

  # Order matters. no-reclaim-account and mobile-cache-flag touch the same lines
  # of spl_kmem_cache_create, and the second only applies after the first.
  mobilityZfsPatches = [
    "dnode-handles-on-linux"
    "dnode-invalidate-on-construct"
    "dnode-move-mutex-window"
    "slab-mobility"
    "arc-hdr-mobility"
    "arc-move-counters"
    "no-reclaim-account"
    "mobile-cache-flag"
    "mobile-cache-flag-userspace"
    "abd-page-mobility"
    "abd-reader-gate"
    "abd-relocate"
    "abd-movable-migratetype"
    "abd-free-gate"
  ];

  mkVariant =
    {
      slabMobility ? false,
      modulePageMobility ? false,
      noReclaimAccount ? false,
      slabMobilityZfs ? false,
      noKswapdWake ? false,
      memProfiling ? false,
    }:
    let
      extraPatches =
        lib.optional slabMobility (kernelPatch "slab-object-mobility")
        ++ lib.optional modulePageMobility (kernelPatch "module-movable-pages");

      # Only wrap the compiler where a kernel is actually built. Wrapping it
      # unconditionally changes the derivation for the unpatched variants too,
      # and those would stop coming out of the binary cache.
      kernel =
        if extraPatches == [ ] && !memProfiling then
          pkgs.linux_latest
        else
          pkgs.linux_latest.override {
            stdenv = ccache.wrapStdenv pkgs.stdenv;
            kernelPatches = pkgs.linux_latest.kernelPatches ++ extraPatches;
            structuredExtraConfig = lib.optionalAttrs memProfiling {
              MEM_ALLOC_PROFILING = lib.kernel.yes;
              MEM_ALLOC_PROFILING_ENABLED_BY_DEFAULT = lib.kernel.yes;
              MEM_ALLOC_PROFILING_DEBUG = lib.kernel.no;
            };
          };

      zfsExtra =
        (
          if slabMobilityZfs then
            map zfsPatch mobilityZfsPatches
          else
            lib.optional noReclaimAccount (zfsPatch "no-reclaim-account")
        )
        ++ lib.optional noKswapdWake (zfsPatch "no-kswapd-wake");

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
  # ABD, so that what is left in a block can be moved out of the way.
  mobility = mkVariant {
    slabMobility = true;
    modulePageMobility = true;
    slabMobilityZfs = true;
  };
}
