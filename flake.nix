{
  description = "Measuring and fixing the memory fragmentation ZFS causes on Linux";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    {
      # The patch series itself, which belongs to no system: a machine that
      # wants to run what this measures takes the files from here rather than
      # keeping a copy that drifts. patches.zfs.relocation is the set to run,
      # patches.zfs.withProbes adds what only answers questions.
      patches = import ./nix/patches.nix;
    }
    //
      # Linux only by nature: the tool reads /proc/kpageflags and the tests build
      # kernels.
      flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" ] (
        system:
        let
          pkgs = import nixpkgs {
            inherit system;
            # nixpkgs marks a ZFS release broken past the newest kernel it was
            # tested against. The point of this repository is the pair that sits
            # just past that line, and it builds and runs.
            config.problems.handlers.zfs.broken = "ignore";
          };
          inherit (pkgs) lib;

          fragview = pkgs.callPackage ./packages/fragview/package.nix { };
          fragload = pkgs.callPackage ./packages/fragload/package.nix { };

          suite = import ./tests {
            inherit
              pkgs
              lib
              fragview
              fragload
              ;
          };
        in
        {
          checks = lib.optionalAttrs (system == "x86_64-linux") suite.checks;

          packages = {
            inherit fragview fragload;
            default = fragview;
          }
          // lib.optionalAttrs (system == "x86_64-linux") {
            # Against the stock kernel, so the module compiles without waiting on
            # a kernel build. The variants carry their own build of it.
            slabwho = pkgs.linuxPackages_latest.callPackage ./packages/slabwho/package.nix { };
          };

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.jq
              pkgs.nixfmt
            ];
          };

        }
      );
}
