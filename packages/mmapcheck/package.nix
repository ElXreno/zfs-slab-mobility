# Readahead into the page cache of a ZFS file: hangs against writers, and bytes.
{ lib, buildGoModule }:

buildGoModule {
  pname = "mmapcheck";
  version = "0.1.0";

  src = lib.cleanSource ./.;
  vendorHash = null;

  meta = {
    description = "Readahead deadlock and byte checks for the ZFS page cache tests";
    mainProgram = "mmapcheck";
    license = lib.licenses.mit;
  };
}
