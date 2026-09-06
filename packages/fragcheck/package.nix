# Checks the set fragload wrote byte for byte, and holds anonymous memory.
{ lib, buildGoModule }:

buildGoModule {
  pname = "fragcheck";
  version = "0.1.0";

  src = lib.cleanSource ./.;
  vendorHash = null;

  meta = {
    description = "Integrity check and memory hog for the fragmentation tests";
    mainProgram = "fragcheck";
    license = lib.licenses.mit;
  };
}
