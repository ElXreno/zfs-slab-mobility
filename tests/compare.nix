{
  pkgs,
  lib,
  fragview,
}:

{
  name,
  # label -> list of run derivations, one per seed.
  runs,
  phase ? "compacted",
  # Column order, and the order assertions read in.
  order ? lib.attrNames runs,
  # { metric, from, to, atMost | atLeast }
  expect ? [ ],
}:

let
  spec =
    e:
    let
      op = if e ? atMost then "<=" else ">=";
      bound = toString (e.atMost or e.atLeast);
    in
    "--expect ${lib.escapeShellArg "${e.metric}:${e.from}->${e.to}${op}${bound}"}";

  dumpsFor =
    label:
    lib.concatStringsSep "\n" (
      lib.imap1 (i: r: ''
        fragview -proc-root ${r}/proc/${phase} -dump "snaps/${label}-${toString i}.snap" \
          -label "${label}/${phase}" -stamp "seed${toString i}" > /dev/null
      '') runs.${label}
    );
in
pkgs.runCommand "compare-${name}"
  {
    nativeBuildInputs = [ fragview ];
    passthru.runs = runs;
  }
  ''
    mkdir -p "$out" snaps

    ${lib.concatMapStringsSep "\n" dumpsFor order}

    fragview -cmp \
      --order ${lib.concatStringsSep "," order} \
      --title ${lib.escapeShellArg name} \
      ${lib.concatMapStringsSep " \\\n      " spec expect} \
      --txt "$out/summary.txt" \
      --md "$out/summary.md" \
      --json "$out/metrics.json" \
      snaps/*.snap

    cp snaps/*.snap "$out/"
  ''
