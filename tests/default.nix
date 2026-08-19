# The suite.
#
# Every check is a comparison of builds, never a single run against a constant:
# absolute counts depend on the machine, the guest size and the workload, while
# the ratio between two builds measured back to back does not.
#
# Three seeds per build. Two would leave a median that is really a mean, and a
# single outlier could then carry a threshold on its own.
{
  pkgs,
  lib,
  fragview,
  fragload,
}:

let
  variants = import ../nix/variants.nix { inherit pkgs lib; };

  mkRun = import ./run.nix {
    inherit
      pkgs
      lib
      variants
      fragload
      ;
  };

  mkCompare = import ./compare.nix { inherit pkgs lib fragview; };

  seeds = [
    1
    2
    3
  ];

  runsForSeeds =
    theseSeeds: args: variant:
    map (seed: mkRun (args // { inherit variant seed; })) theseSeeds;
  runsFor = runsForSeeds seeds;

  checks = {
    # The one line change, asserted on the slab pages left inside nearly empty
    # blocks. Counting blocks with more than one immovable owner says less once
    # the ARC holds most of memory: nearly every block has an ARC page then.
    separation = mkCompare {
      name = "separation";
      order = [
        "stock"
        "separation"
      ];
      runs = {
        stock = runsFor { } "stock";
        separation = runsFor { } "separation";
      };
      expect = [
        {
          metric = "slab_in";
          from = "stock";
          to = "separation";
          atMost = 0.3;
        }
        {
          metric = "pinned";
          from = "stock";
          to = "separation";
          atMost = 0.6;
        }
      ];
    };

    # Object relocation and movable ABD pages. At this guest size the outcome is
    # not reliably better than separation alone, so what is asserted is that the
    # patches do what they claim: the scatter ABD ends up in movable pageblocks
    # instead of unmovable ones. Whether that turns into fewer hostage blocks
    # depends on how much there is to move, which is a property of the workload.
    mobility = mkCompare {
      name = "mobility";
      order = [
        "separation"
        "mobility"
      ];
      runs = {
        separation = runsFor { } "separation";
        mobility = runsFor { } "mobility";
      };
      expect = [
        {
          metric = "blocks_movable";
          from = "separation";
          to = "mobility";
          atLeast = 1.5;
        }
        # A guard, not a claim. The seeds favour mobility, 155/181/211 MiB
        # against 241/257/397, but part of that gap is the two builds no longer
        # agreeing on what counts as immovable, which is what the patch changes.
        # Kept loose so it catches mobility breaking something outright.
        {
          metric = "pinned";
          from = "separation";
          to = "mobility";
          atMost = 1.5;
        }
      ];
    };

    kvmem = mkCompare {
      name = "kvmem";
      phase = "reread";
      order = [
        "separation"
        "nokswapd"
      ];
      runs =
        let
          # recordsize only caps the block size: a file smaller than it still
          # gets one block its own size. Reaching the caches that grow through
          # vmalloc needs files at least as large as the record.
          # zio_buf_comb_* holds linear buffers, and reading an uncompressed
          # record needs none: the data goes straight into a scatter ABD. The
          # data is random, so compression stores it whole and only the
          # decompression buffer is added.
          bigRecords = {
            recordSize = "1m";
            compression = "zstd-3";
            fileSize = 1048576;
            files = 6000;
            # Each reader in flight holds a megabyte linear buffer, and those
            # come from the cache whose slabs are the order 10 request. Four
            # readers asked for fewer of them than the guest had left.
            readJobs = 32;
            # Demand has to outrun supply at the moment of the request. The
            # first pass leaves about eighty order 10 blocks free, so the burst
            # is sized to need more slabs than that.
            burstJobs = 512;
          };
        in
        {
          separation = runsFor bigRecords "separation";
          nokswapd = runsFor bigRecords "nokswapd";
        };
      expect = [
        {
          metric = "kswapd_scan";
          from = "separation";
          to = "nokswapd";
          atMost = 0.5;
          # Calibrated from both ends. On a shared runner the baseline came
          # out 1, 1 and 93 across three runs and the direction flipped: once
          # the patched variant scanned 83 pages against 1, once 0 against 93.
          # On a machine where the phenomenon is real it came out 493 against
          # 43, with the arc 8.5% larger for it. The floor sits between the
          # two bands, well above the noise and well below the finding.
          floor = 250;
          # A runner with memory to spare never puts kswapd to work, so this
          # can only ever read as no signal there. Skipping says that plainly;
          # failing would report the host as a regression.
          skipNoSignal = true;
        }
        # The consequence rather than the indicator: what kswapd reclaims when
        # woken is the ARC, which is what the cache was being grown to serve.
        # Gated on the cause, because an ARC that was never squeezed is not
        # evidence that the patch preserved it.
        {
          metric = "arc_size";
          from = "separation";
          to = "nokswapd";
          atLeast = 1.05;
          gate = "kswapd_scan";
          # The same band, for the same reason: while the cause was at runner
          # size the arc held within a percent and a half either way, which is
          # the noise this asks to see through.
          floor = 250;
          skipNoSignal = true;
        }
      ];
    };

    # The outcome the patches exist for, in the allocator's own currency. Every
    # other check counts blocks and pages, which are means; this one asks how
    # much contiguous memory the machine can still hand out with the ARC full.
    #
    # Measured before asserted, and the measurement has a ceiling. Ask for a
    # third of the guest and the demand is met out of chunks the ARC holds,
    # which is the thing under test; ask for half and the request itself
    # evicts the ARC, so what gets handed out tracks how far the ARC happened
    # to fall rather than whether its chunks could move, and mobility with a
    # full ARC then loses to separation with a collapsed one. So the demand
    # stays at a third and the noise is answered with seeds instead: five
    # rather than three, so a single lucky run moves the median less.
    highorder = mkCompare {
      name = "highorder";
      phase = "highorder";
      order = [
        "separation"
        "mobility"
      ];
      runs =
        let
          # A third of the guest. More turns the request into an eviction and
          # measures the ARC's collapse instead of the chunks' mobility.
          demand = {
            hugeDemand = 1024;
          };
          fiveSeeds = runsForSeeds [ 1 2 3 4 5 ] demand;
        in
        {
          separation = fiveSeeds "separation";
          mobility = fiveSeeds "mobility";
        };
      expect = [
        {
          metric = "hugepages";
          from = "separation";
          to = "mobility";
          atLeast = 1.5;
        }
      ];
    };

    # Relocation of chunks larger than one page, which is what the machine this
    # was written on allocates almost exclusively. Not a comparison: the thing
    # asserted is that the kernel does not corrupt a list while migrating, so
    # the run is the check and the assertions live in it.
    #
    # One seed rather than three. A comparison needs a median because the
    # quantity moves between runs; this either walks into the bug or does not,
    # and a second seed would buy nothing for another guest.
    #
    # Only the mobility build. Separation marks no page movable, so compaction
    # would find nothing of ours and could not fail here however long it ran.
    abd-migrate = mkRun {
      # The probe build: this check asks what relocation did, not how big the
      # slab ended up, so the probe's cost to the dbuf cache does not distort
      # what it reads.
      variant = "probes";
      seed = 1;
      compactWhileWarm = true;
    };

    # vm.defrag_mode tells the allocator to compact rather than mix migrate types.
    # That is worth something only if the pages it wants to compact can move, so
    # on a stock kernel it should make no measurable difference either way.
    defrag-mode = mkCompare {
      name = "defrag-mode";
      order = [
        "on"
        "off"
      ];
      runs = {
        on = runsFor { defragMode = 1; } "stock";
        off = runsFor { defragMode = 0; } "stock";
      };
      expect = [
        {
          metric = "pinned";
          from = "on";
          to = "off";
          atMost = 1.15;
        }
        {
          metric = "pinned";
          from = "on";
          to = "off";
          atLeast = 0.85;
        }
      ];
    };
  };
in
{
  inherit checks;
}
