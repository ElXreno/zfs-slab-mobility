# Object caching for the patched kernel builds.
#
# A build here never evicts anything: CCACHE_MAXSIZE is zero, which ccache reads
# as no limit. Trimming belongs to whoever owns the directory.
#
# Saying nothing at all does not work. The sandbox only carries the derivation's
# closure, so a ccache.conf that is a symlink into the store, which is how a
# NixOS module would place one, dangles inside the build. ccache then falls back
# to its built in five gigabyte default and starts evicting a cache many times
# that size on every compile.
#
# ccache does not degrade politely on its own either: with the directory absent
# from the sandbox it fails the compile with "ccache: error: Permission denied".
# The guard below is what keeps the build working for anyone who has not set the
# directory up.
#
#     nix.settings.extra-sandbox-paths = [ "/nix/ccache" ];
#     systemd.tmpfiles.rules = [ "d /nix/ccache 0777 root root -" ];
{ pkgs }:

let
  cacheDir = "/nix/ccache";
in
{
  inherit cacheDir;

  # Replaces the unwrapped compiler rather than the wrapper: kbuild passes CC as
  # an absolute path to the unwrapped one.
  wrapStdenv =
    stdenv:
    pkgs.stdenvAdapters.overrideCC stdenv (
      stdenv.cc.override {
        cc = pkgs.ccache.links {
          unwrappedCC = stdenv.cc.cc;
          extraConfig = ''
            export CCACHE_DIR="${cacheDir}"
            export CCACHE_COMPILERCHECK="string:${stdenv.cc.cc}"
            export CCACHE_SLOPPINESS="include_file_mtime,include_file_ctime,time_macros,locale,random_seed"
            export CCACHE_MAXSIZE=0
            if [ ! -w "$CCACHE_DIR" ]; then export CCACHE_DISABLE=1; fi
          '';
        };
      }
    );
}
