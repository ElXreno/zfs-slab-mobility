# Reads /proc/kpageflags and paints what every pageblock is made of. Needs root
# on a live machine; on a snapshot it needs nothing at all.
{ lib, buildGoModule }:

buildGoModule {
  pname = "fragview";
  version = "0.1.0";

  src = lib.cleanSource ./.;
  vendorHash = "sha256-uwBJAqN4sIepiiJf9lCDumLqfKJEowQO2tOiSWD3Fig=";

  meta = {
    description = "Physical memory fragmentation viewer with snapshots";
    mainProgram = "fragview";
    license = lib.licenses.mit;
  };
}
