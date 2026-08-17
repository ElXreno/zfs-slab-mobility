# Answers the question /proc/slabinfo does not: how many pageblocks does each
# cache touch, and how many of those are nearly empty.
#
# It mirrors `struct kmem_cache`, which is private to the allocator, so it
# tracks one kernel version at a time and refuses to load rather than print
# nonsense when the layout it expects is not the one in front of it.
{
  lib,
  stdenv,
  kernel,
}:

stdenv.mkDerivation {
  pname = "slabwho";
  version = "0.1.0";

  src = lib.cleanSource ./.;

  nativeBuildInputs = kernel.moduleBuildDependencies;
  hardeningDisable = [ "pic" ];

  makeFlags = [
    "-C"
    "${kernel.dev}/lib/modules/${kernel.modDirVersion}/build"
    "M=$(PWD)"
    "modules"
  ];

  installPhase = ''
    install -Dm444 slabwho.ko "$out/lib/modules/${kernel.modDirVersion}/extra/slabwho.ko"
  '';

  meta = {
    description = "Per cache pageblock accounting for SLUB";
    license = lib.licenses.gpl2Only;
  };
}
