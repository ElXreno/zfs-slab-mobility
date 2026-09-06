# The patch series as plain data, so that the stand and any machine running
# what it measures apply the same files in the same order. Nothing here needs
# a package set, which is what lets the flake hand it out directly.
let
  zfsFile = name: ../patches/zfs/${name}.patch;
  kernelFile = name: ../patches/kernel/${name}.patch;

  # Order matters. no-reclaim-account and mobile-cache-flag touch the same
  # lines of spl_kmem_cache_create, and the second only applies after the
  # first.
  relocation = [
    "dnode-handles-on-linux"
    "dnode-invalidate-on-construct"
    "dnode-move-mutex-window"
    "slab-mobility"
    "arc-hdr-mobility"
    "arc-move-counters"
    "no-reclaim-account"
    "mobile-cache-flag"
    "mobile-cache-flag-userspace"
    "mobile-cache-rcu"
    "arc-move-disable"
    "eio-diag"
    "arc-hdr-accounting"
  ];

  # Header relocation on the folio backend; the abd-* series is replaced by the
  # page cache, which pins, moves and reclaims the data pages itself.
  arclru = [
    "dnode-handles-on-linux"
    "dnode-invalidate-on-construct"
    "dnode-move-mutex-window"
    "slab-mobility"
    "arc-hdr-mobility"
    "arc-move-counters"
    "no-reclaim-account"
    "mobile-cache-flag"
    "mobile-cache-flag-userspace"
    "mobile-cache-rcu"
    "arc-move-disable"
    "eio-diag"
    "arc-hdr-accounting"
    "arc-folio-lru"
  ];

  # The one probe that still applies on the folio backend.
  arclruProbes = [ "dbuf-move-probe" ];

  # What only answers a question. dbuf-move-probe creates the dbuf cache
  # mobile, and that flag costs the cache its per CPU sheaves and its merging
  # on a hot allocator path, so it is not something to carry on a machine for
  # a measurement the stand has already taken.
  probes = [
    "dbuf-move-probe"
  ];

  # Applied on their own rather than as part of a series, to isolate one
  # change against an otherwise stock build. no-reclaim-account appears in the
  # series as well, which is why it is not repeated here.
  alone = [
    "no-kswapd-wake"
  ];

  named = relocation ++ probes ++ alone ++ [ "arc-folio-lru" ];

  byName = builtins.listToAttrs (
    map (name: {
      inherit name;
      value = zfsFile name;
    }) named
  );

  # Every patch on disk has to be named above, or a series would quietly
  # differ from what the directory holds. Caught here rather than at whatever
  # rebuild first notices the file went nowhere.
  unnamed = builtins.filter (
    file: !(builtins.elem (builtins.replaceStrings [ ".patch" ] [ "" ] file) named)
  ) (builtins.attrNames (builtins.readDir ../patches/zfs));
in
if unnamed != [ ] then
  throw "patches/zfs holds files no list in nix/patches.nix names: ${builtins.concatStringsSep ", " unnamed}"
else
  {
    zfs = {
      # What makes relocation work and keeps it safe: for a machine.
      relocation = map zfsFile relocation;

      # The above plus the probes: for the stand.
      withProbes = map zfsFile (relocation ++ probes);

      # The folio backend with the header relocation it keeps: for a machine.
      arclru = map zfsFile arclru;
      arclruWithProbes = map zfsFile (arclru ++ arclruProbes);

      # Reachable one at a time, for a build that wants a single change.
      each = byName;

      names = { inherit relocation arclru probes alone; };
    };

    kernel = {
      slab-object-mobility = kernelFile "slab-object-mobility";
      filemap-exports = kernelFile "filemap-exports";
      compaction-large-folio = kernelFile "compaction-large-folio";
    };
  }
