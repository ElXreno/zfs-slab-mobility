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

  runsFor = args: variant: map (seed: mkRun (args // { inherit variant seed; })) seeds;

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
        {
          metric = "pinned";
          from = "separation";
          to = "mobility";
          atMost = 1.1;
        }
      ];
    };

    kvmem = mkCompare {
      name = "kvmem";
      order = [
        "separation"
        "nokswapd"
      ];
      runs =
        let
          # recordsize only caps the block size: a file smaller than it still
          # gets one block its own size. Reaching the caches that grow through
          # vmalloc needs files at least as large as the record.
          bigRecords = {
            recordSize = "1m";
            fileSize = 1048576;
            files = 6000;
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
        }
      ];
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
