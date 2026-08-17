# Writes and reads a file set that is the same on every run for a given seed.
{ lib, buildGoModule }:

buildGoModule {
  pname = "fragload";
  version = "0.1.0";

  src = lib.cleanSource ./.;
  vendorHash = null;

  meta = {
    description = "Deterministic ZFS file load for fragmentation tests";
    mainProgram = "fragload";
    license = lib.licenses.mit;
  };
}
