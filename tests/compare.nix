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
  # { metric, from, to, atMost | atLeast, floor ?, ceiling ?, gate ?, skipNoSignal ? }
  # floor is the smallest baseline the ratio may be taken from: below it the
  # comparison reports no signal instead of dividing noise by noise. gate names
  # the metric that floor applies to, for asserting on a consequence whose
  # cause is measured elsewhere; it defaults to the asserted metric.
  # skipNoSignal lets a no-signal result skip rather than fail the run, for a
  # check whose phenomenon a shared CI runner cannot produce. ceiling is the
  # floor from the other end, for a metric capped by what the run asked for: a
  # baseline already at the cap had nothing to be improved.
  expect ? [ ],
}:

let
  spec =
    e:
    let
      op = if e ? atMost then "<=" else ">=";
      bound = toString (e.atMost or e.atLeast);
      gate =
        lib.optionalString (
          (e ? floor) || (e ? ceiling)
        ) "@${e.gate or e.metric}:${toString (e.floor or 0)}"
        + lib.optionalString (e ? ceiling) ":${toString e.ceiling}";
      skip = lib.optionalString (e.skipNoSignal or false) "?skip";
    in
    "--expect ${lib.escapeShellArg "${e.metric}:${e.from}->${e.to}${skip}${op}${bound}${gate}"}";

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
