# Streams large files through mmap and takes shared memory in a burst.
{ lib, buildGoModule }:

buildGoModule {
  pname = "fragspike";
  version = "0.1.0";

  src = lib.cleanSource ./.;
  vendorHash = null;

  meta = {
    description = "Model load against a full page cache, for the fragmentation tests";
    mainProgram = "fragspike";
    license = lib.licenses.mit;
  };
}
