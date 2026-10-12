#!/usr/bin/env python3
"""Generate docs/figures/rtc-media-bench-flow.html.

Measured, not positioned by eye: every box is sized from its own text, so editing a line below
cannot overlap anything. Same shape as the generators in this portfolio, for the same reason.

    python3 docs/figures/rtc-media-bench-flow.generator.py
    python3 <diagram-design>/scripts/self_check.py docs/figures/rtc-media-bench-flow.html
    make diagram
"""
import html
import pathlib

PAPER, INK, MUTED, ACCENT, LINE, TINT, BAD = "#f5f5f5", "#2d3142", "#4f5d75", "#b3541e", "#c9ccd1", "#fdf4ef", "#8c2f1f"
FONT = {"sans": "'Geist', system-ui, sans-serif",
        "mono": "'Geist Mono', ui-monospace, monospace",
        "serif": "'Instrument Serif', serif"}
ADVANCE = {"mono": 0.605, "sans": 0.525, "sans-bold": 0.565}
PAD, GAP = 16, 110


def esc(s):
    return html.escape(str(s))


def width(s, size, family="sans", weight="400"):
    key = "mono" if family == "mono" else ("sans-bold" if weight == "600" else "sans")
    return len(s) * ADVANCE[key] * size


def text(x, y, s, size=12.5, weight="400", fill=INK, family="sans", anchor="start"):
    return (f'<text x="{x:.0f}" y="{y:.0f}" font-family="{FONT[family]}" font-size="{size}" '
            f'font-weight="{weight}" fill="{fill}" text-anchor="{anchor}">{esc(s)}</text>')


def rect(x, y, w, h, fill="#ffffff", stroke=LINE, rx=10):
    return f'<rect x="{x:.0f}" y="{y:.0f}" width="{w:.0f}" height="{h:.0f}" rx="{rx}" fill="{fill}" stroke="{stroke}"/>'


def arrow(x1, y1, x2, y2, label="", dashed=False):
    dash = ' stroke-dasharray="5 4"' if dashed else ""
    out = [f'<line x1="{x1:.0f}" y1="{y1:.0f}" x2="{x2:.0f}" y2="{y2:.0f}" stroke="{MUTED}" stroke-width="1.6" '
           f'marker-end="url(#arrowhead)"{dash}/>']
    if label:
        mx, my = (x1 + x2) / 2, (y1 + y2) / 2
        lw = width(label, 11, "mono") + 12
        out.append(rect(mx - lw / 2, my - 12, lw, 20, fill=PAPER, stroke="none", rx=5))
        out.append(text(mx, my + 2, label, size=11, fill=MUTED, family="mono", anchor="middle"))
    return "".join(out)


class Block:
    def __init__(self, title, subtitle, lines, tint=None):
        self.title, self.subtitle, self.lines, self.tint = title, subtitle, lines, tint
        widths = [width(title, 15, weight="600")]
        if subtitle:
            widths.append(width(subtitle, 11.5, "mono"))
        for line, mono in lines:
            widths.append(width(line, 11.5, "mono" if mono else "sans"))
        self.w = max(widths) + PAD * 2
        self.h = PAD * 2 + 22 + (16 if subtitle else 0) + len(lines) * 17

    def render(self, x, y):
        out = [rect(x, y, self.w, self.h, fill=self.tint or "#ffffff")]
        out.append(text(x + PAD, y + PAD + 12, self.title, size=15, weight="600"))
        yy = y + PAD + 34
        if self.subtitle:
            out.append(text(x + PAD, yy + 4, self.subtitle, size=11.5, fill=MUTED, family="mono"))
            yy += 16
        for line, mono in self.lines:
            out.append(text(x + PAD, yy + 9, line, size=11.5, family="mono" if mono else "sans"))
            yy += 17
        return "".join(out)


TITLE = Block("the legacy title", "Admin API over HTTP, or a title export on disk", [
    ("GetPlayersInSegment    paged, 429-aware", True),
    ("GetUserData / Inventory / Currency", True),
    ("Statistics / Profile   5 calls per player", True),
    ("internal/source  <- two implementations,", True),
    ("one interface: the export needs no calls", True),
])
MAPPING = Block("identity, then meaning", "the two decisions everything else rests on", [
    ("custom_id = UUIDv5(playfab, legacy_id)", True),
    ("namespace frozen: changing it does not", True),
    ("merge accounts, it creates them again", True),
    ("checksum per document, over canonical", True),
    ("bytes: key order, timestamps, inventory", True),
])
LOOP = Block("the importer", "read, map, derive, write, record", [
    ("-concurrency=4   players in flight", True),
    ("progress is a checkpoint, not a guess", True),
    ("-dry-run   asks the target and writes", True),
    ("nothing anywhere, including the ledger", True),
    ("cli: importer  ·  reconcile", True),
])
KITDB = Block("the kit's own PostgreSQL", "bookkeeping, not data", [
    ("ledger    append-only: what was written", True),
    ("checkpoint  where the listing stopped", True),
    ("identity_map  unique on custom_id, so", True),
    ("two source ids cannot claim one account", True),
    ("migrate    numbered SQL, schema_version", True),
])
TARGET = Block("Nakama, and the module that owns the writes", "the importer never touches storage directly", [
    ("migration.apply   write + record the", True),
    ("checksum in one storage write", True),
    ("migration.plan    the same code path,", True),
    ("writes withheld", True),
    ("migration.inspect / status   read back", True),
    ("called with runtime.http_key, not the", True),
    ("server key   (that answers HTTP key invalid)", True),
])
RESULT = Block("what a second run does", "measured: 10,000 players", [
    ("run 1   47,500 documents   41.4 s", True),
    ("run 2   0 written, 47,500 skipped   27.8 s", True),
    ("the 27.8 s is the source being read,", True),
    ("not the target being written to", True),
])
RECONCILE = Block("reconcile: the same question, from outside", "checksums, never bytes", [
    ("source -> mapping -> expected checksum", True),
    ("target -> migration.inspect -> recorded", True),
    ("Nakama re-serialises {\"gold\": 600} with a", True),
    ("space: comparing bytes reports every", True),
    ("player as changed", True),
    ("exits non-zero when anything is missing", True),
    ("or changed: that is what makes it a gate", True),
])
CRASH = Block("killed mid-batch", "make demo-crash, SIGKILL after a checkpoint", [
    ("crashed after 200 players, 983 documents", True),
    ("the next run   identical 7   <- those seven", True),
    ("had every document written before the kill", True),
    ("finished   2,000 players   0 checkpoints left", True),
], tint="#fdf4ef")
CLAIMS = [
    "a second run wrote 0 of 47,500 documents",
    "the checksum lives where the data lives",
    "a finished run clears its checkpoint",
]

col1 = max(TITLE.w, MAPPING.w)
col2 = max(LOOP.w, KITDB.w)
col3 = max(TARGET.w, RESULT.w, RECONCILE.w, CRASH.w)
MARGIN, TOP = 28, 74
x1, x2 = MARGIN, MARGIN + col1 + GAP
x3 = x2 + col2 + GAP
total_w = x3 + col3 + MARGIN

y = TOP
title_y, mapping_y = y + 20, y + 20 + TITLE.h + 40
loop_y, kitdb_y = y, y + LOOP.h + 40
target_y = y
result_y = target_y + TARGET.h + 40
reconcile_y = result_y + RESULT.h + 40
crash_y = reconcile_y + RECONCILE.h + 40
strip_y = max(crash_y + CRASH.h, mapping_y + MAPPING.h, kitdb_y + KITDB.h) + 42
strip_h = 78
H = strip_y + strip_h + 46

svg = [f'<svg viewBox="0 0 {total_w:.0f} {H:.0f}" role="img" aria-labelledby="fig-title fig-desc" xmlns="http://www.w3.org/2000/svg">']
svg.append('<title id="fig-title">nakama-migration-kit: where a player\'s data goes, and why the second run writes nothing</title>')
svg.append('<desc id="fig-desc">A legacy title is read through one of two Source implementations behind a single interface, '
           'a live Admin API or a title export on disk. Each player gets a custom id derived as a UUIDv5 of the legacy id, so a '
           're-run computes the same account instead of creating a second one, and each document gets a checksum of its meaning '
           'rather than of its bytes. The importer maps the record, derives the identity and calls the runtime module, which '
           'writes the document and the checksum it wrote it for in one storage write, so a run that dies between the write and '
           'its own bookkeeping is still idempotent. The kit\'s own PostgreSQL holds the append-only ledger and the checkpoint. '
           'Measured on ten thousand players: the first run writes 47,500 documents in 41.4 seconds, the second writes none and '
           'skips 47,500 in 27.8 seconds because it still reads every player. Reconciliation asks the same question from outside, '
           'comparing checksums rather than bytes because Nakama re-serialises what it stores, and exits non-zero when anything '
           'is missing or changed. A run killed after 200 players and 983 documents resumes and finds 7 players already complete.</desc>')
svg.append(f'<defs><marker id="arrowhead" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" '
           f'orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="{MUTED}"/></marker></defs>')

svg.append(TITLE.render(x1, title_y))
svg.append(MAPPING.render(x1, mapping_y))
svg.append(LOOP.render(x2, loop_y))
svg.append(KITDB.render(x2, kitdb_y))
svg.append(TARGET.render(x3, target_y))
svg.append(RESULT.render(x3, result_y))
svg.append(RECONCILE.render(x3, reconcile_y))
svg.append(CRASH.render(x3, crash_y))

svg.append(arrow(x1 + col1, title_y + 46, x2, loop_y + 40, "players"))
svg.append(arrow(x2 + col2, loop_y + 40, x3, target_y + 40, "migration.apply"))
svg.append(arrow(x3, result_y + 30, x2 + col2, kitdb_y + 40, "what was written"))
svg.append(text(MARGIN, H - 16, "make demo  ·  make demo-crash  ·  make smoke  — every number here comes from one of these",
                size=11, fill=MUTED, family="mono"))
svg.append(rect(MARGIN, strip_y, total_w - MARGIN * 2, strip_h))
svg.append(text(MARGIN + PAD, strip_y + 26, "three rules this kit keeps, each one because breaking it produced a wrong number",
                size=12.5, weight="600", fill=MUTED))
chip_w = (total_w - MARGIN * 2 - PAD * 2 - 24) / 3
# A generator that clips a claim is a generator that ships a figure nobody can read, and the clipping
# is invisible in the HTML: it only shows in the export, which is the artefact people look at. This
# fails instead, naming the claim that does not fit.
for i, claim in enumerate(CLAIMS):
    need = width(f"{i + 1}. {claim}", 11.5) + 20
    if need > chip_w:
        raise SystemExit(f"claim {i + 1} needs {need:.0f}px, the strip gives {chip_w:.0f}px: shorten it")
for i, claim in enumerate(CLAIMS):
    cx = MARGIN + PAD + i * (chip_w + 12)
    svg.append(rect(cx, strip_y + 38, chip_w, 28, fill=TINT, stroke="#e8cdbd", rx=6))
    svg.append(text(cx + 10, strip_y + 57, f"{i + 1}. {claim}", size=11.5))
svg.append("</svg>")

meta = ('<!-- diagram-meta {"topic":"nakama-migration-kit-flow","level":"runtime","type":"data-flow",'
        '"question":"Where does a player\'s data go, and why does a second run write nothing?",'
        '"sources":["internal/source/source.go","internal/identity/identity.go","internal/mapping/mapping.go",'
        '"internal/canon/canon.go","internal/ledger/ledger.go","internal/checkpoint/checkpoint.go",'
        '"internal/target/target.go","internal/importer/importer.go","internal/reconcile/reconcile.go",'
        '"runtime/cmd/server/main.go","cmd/importer/main.go","cmd/reconcile/main.go","scripts/demo.sh",'
        '"scripts/demo-crash.sh","README.md"],"updated":"2026-10-12"} -->')

page = f"""{meta}
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>nakama-migration-kit</title>
<link href="https://fonts.googleapis.com/css2?family=Instrument+Serif:ital@0;1&family=Geist:wght@400;500;600&family=Geist+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<style>
  *, *::before, *::after {{ box-sizing: border-box; margin: 0; padding: 0; }}
  :root {{
    --color-paper: {PAPER}; --color-ink: {INK}; --color-muted: {MUTED}; --color-accent: {ACCENT};
    --font-sans: 'Geist', system-ui, sans-serif; --font-serif: 'Instrument Serif', serif;
    --font-mono: 'Geist Mono', ui-monospace, monospace;
  }}
  body {{ font-family: var(--font-sans); background: var(--color-paper); color: var(--color-ink);
         min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 3rem 2rem; }}
  .frame {{ max-width: {total_w + 40:.0f}px; width: 100%; }}
  .eyebrow {{ font-family: var(--font-mono); font-size: 0.66rem; font-weight: 500; letter-spacing: 0.18em;
              text-transform: uppercase; color: var(--color-muted); margin-bottom: 0.5rem; }}
  h1 {{ font-family: var(--font-serif); font-size: clamp(1.5rem, 2.4vw + 0.75rem, 2rem); font-weight: 400;
        letter-spacing: -0.02em; line-height: 1.15; color: var(--color-ink); margin-bottom: 1.5rem; }}
  .diagram-container {{ width: 100%; overflow-x: auto; }}
  svg {{ width: 100%; max-width: {total_w:.0f}px; display: block; }}
  .caption {{ font-family: var(--font-mono); font-size: 0.7rem; color: var(--color-muted); margin-top: 1rem; }}
</style>
</head>
<body>
<div class="frame">
  <p class="eyebrow">nakama-migration-kit · import, reconcile, resume</p>
  <h1>Where a player's data goes, and why the second run writes nothing</h1>
  <div class="diagram-container">
{"".join(svg)}
  </div>
  <p class="caption">The write path and the record of it are the same storage write, which is why a run that dies between
  them is still idempotent: the next run does not ask the importer what it did, it asks the target. The second run takes
  27.8 seconds against the first run's 41.4 and writes nothing - it is not faster because it skipped the work, it is the
  same work with the writes withheld, and that is the difference between a claim and a slogan.</p>
</div>
</body>
</html>
"""
out = pathlib.Path(__file__).with_name("nakama-migration-kit-flow.html")
out.write_text(page, encoding="utf-8")
print(f"wrote {out.name}: {total_w:.0f}x{H:.0f} viewBox, {len(page)} bytes")
