"""Draw every Tentmaker mark, the outlined wordmark and the icons.

Usage: uv run --no-project --with fonttools --with uharfbuzz python brand/tools/gen.py FONT OUTDIR
FONT is Bricolage Grotesque's variable TTF (see brand/README.md for the pinned URL).
"""
import math, sys, os

FONT, OUT = sys.argv[1], sys.argv[2]
os.makedirs(OUT, exist_ok=True)

MADDER, MADDER_LIGHT = "#A63D2A", "#E07A5F"
CILICIUM, LINEN = "#1F1C19", "#F3EEE3"

def P(*pts):
    return "M" + "L".join(f"{x:.2f} {y:.2f}" for x, y in pts) + "Z"

def lerp(a, b, t):
    return (a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t)

def tent(F, Fl, Fr, B, k, gaps, g, sep, door):
    """A tent in three-quarter view: the door panel F-Fr-Fl in front, the side
    roof running back to B, woven in strips (gaps along the ridge)."""
    Br = (B[0] + k * (Fr[0] - F[0]), B[1] + k * (Fr[1] - F[1]))
    dx, dy = Fr[0] - F[0], Fr[1] - F[1]
    n = math.hypot(dx, dy)
    nx, ny = dy / n, -dx / n
    off = lambda p: (p[0] + nx * sep, p[1] + ny * sep)
    edges = [0] + [x for t in gaps for x in (t - g / 2, t + g / 2)] + [1]
    side = []
    for i in range(0, len(edges), 2):
        a, b = edges[i], edges[i + 1]
        side.append(P(off(lerp(F, Fr, a)), lerp(B, Br, a), lerp(B, Br, b), off(lerp(F, Fr, b))))
    cx = (Fl[0] + Fr[0]) / 2
    if door:
        top, hw = door
        front = P(F, Fr, (cx + hw, Fr[1]), (cx, top), (cx - hw, Fl[1]), Fl)
    else:
        front = P(F, Fr, Fl)
    return front, " ".join(side)

# Full mark: three woven strips, 32 px and up. Small cut: one seam, 16 to 24 px.
FULL = tent((21, 12), (3, 54), (39, 54), (47, 9.5), 0.74, (0.355, 0.665), 0.05, 1.9, (34, 7))
SMALL = tent((20, 9), (1, 58), (39, 58), (46, 6.5), 0.72, (0.5,), 0.09, 2.8, (38, 8))

def mark_body(m, front, side):
    return f'<path d="{m[0]}" fill="{front}"/><path d="{m[1]}" fill="{side}"/>'

def svg(body, w=64, h=64, vb=None, title="Tentmaker"):
    vb = vb or f"0 0 {w} {h}"
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" viewBox="{vb}" '
            f'role="img" aria-label="{title}"><title>{title}</title>{body}</svg>\n')

def write(name, text):
    with open(os.path.join(OUT, name), "w") as f:
        f.write(text)

write("mark.svg", svg(mark_body(FULL, MADDER, CILICIUM)))
write("mark-on-dark.svg", svg(mark_body(FULL, MADDER_LIGHT, LINEN)))
write("mark-mono.svg", svg(mark_body(FULL, "currentColor", "currentColor")))
write("mark-small.svg", svg(mark_body(SMALL, MADDER, CILICIUM)))
write("mark-small-on-dark.svg", svg(mark_body(SMALL, MADDER_LIGHT, LINEN)))

def tile(m, size, rx, pad, bg=CILICIUM, front=MADDER_LIGHT, side=LINEN, dy=0):
    k = (size - 2 * pad) / 64
    return svg(f'<rect width="{size}" height="{size}" rx="{rx}" fill="{bg}"/>'
               f'<g transform="translate({pad} {pad + dy}) scale({k:.4f})">{mark_body(m, front, side)}</g>',
               size, size)

# The mark's ink box is x 3..60.3, y 9.5..54: nudge it to the optical center.
write("icon.svg", tile(FULL, 1024, 228, 150, dy=-6))
write("icon-full-bleed.svg", tile(FULL, 1024, 0, 170, dy=-6))
write("favicon.svg", tile(SMALL, 64, 14, 6))

# Wordmark: "tentmaker" in Bricolage Grotesque Bold, shaped by HarfBuzz and outlined.
import uharfbuzz as hb
from fontTools.ttLib import TTFont
from fontTools.varLib.instancer import instantiateVariableFont
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen

AXES = {"wght": 700, "opsz": 96, "wdth": 100}
TRACK = -0.012  # em; tighter makes "nt" and "ma" touch

def outline(text, axes=AXES, track_em=TRACK):
    blob = hb.Blob.from_file_path(FONT)
    face = hb.Face(blob)
    font = hb.Font(face)
    font.set_variations(axes)
    buf = hb.Buffer()
    buf.add_str(text)
    buf.guess_segment_properties()
    hb.shape(font, buf, {"kern": True, "liga": False})
    tt = instantiateVariableFont(TTFont(FONT), axes)
    gs = tt.getGlyphSet()
    upem = tt["head"].unitsPerEm
    order = tt.getGlyphOrder()
    pen = SVGPathPen(gs)
    x = 0
    track = track_em * upem
    for info, pos in zip(buf.glyph_infos, buf.glyph_positions):
        name = order[info.codepoint]
        tp = TransformPen(pen, (1, 0, 0, -1, x + pos.x_offset, -pos.y_offset))
        gs[name].draw(tp)
        x += pos.x_advance + track
    x -= track
    os2 = tt["OS/2"]
    return pen.getCommands(), x, upem, os2.sTypoAscender, os2.sxHeight

d, adv, upem, asc, xh = outline("tentmaker")

def lockup(front, side, ink, title="Tentmaker"):
    # Type at 100 units per em. The mark's base sits on the baseline and its top
    # rises above the ascenders, as on the brand sheet.
    s = 100 / upem
    mark_h = 100 * 1.12          # mark viewBox height in lockup units
    k = mark_h / 64
    mark_w = 64 * k
    gap = 14
    base = 84                    # baseline y
    mx = -3 * k                  # trim the mark's left side bearing
    my = base - 54 * k           # mark bottom (y 54) on the baseline
    wx = mx + 60.3 * k + gap
    w = wx + adv * s + 2
    top = my + 9.5 * k - 2
    h = base + 26
    body = (f'<g transform="translate({mx:.2f} {my:.2f}) scale({k:.4f})">{mark_body(FULL, front, side)}</g>'
            f'<path transform="translate({wx:.2f} {base}) scale({s:.5f})" d="{d}" fill="{ink}"/>')
    return svg(body, round(w), round(h - top), f"0 {top:.2f} {w:.2f} {h - top:.2f}", title)

write("wordmark.svg", lockup(MADDER, CILICIUM, CILICIUM))
write("wordmark-on-dark.svg", lockup(MADDER_LIGHT, LINEN, LINEN))

# Social card, 1280 by 640 (GitHub's social preview size).
def text_path(t, size, axes, fill, x, y, track_em=0):
    d2, adv2, upem2, _, _ = outline(t, axes, track_em)
    s2 = size / upem2
    return f'<path transform="translate({x} {y}) scale({s2:.5f})" d="{d2}" fill="{fill}"/>', adv2 * s2

BODY = {"wght": 500, "opsz": 32, "wdth": 100}
SMALLTXT = {"wght": 500, "opsz": 14, "wdth": 100}
W, H = 1280, 640
big = 7.4  # the backdrop mark's scale
parts = [f'<rect width="{W}" height="{H}" fill="{CILICIUM}"/>',
         f'<g transform="translate({W - 64 * big * 0.62:.1f} {H - 54 * big * 0.86:.1f}) scale({big})" opacity="0.07">'
         f'{mark_body(FULL, LINEN, LINEN)}</g>']
s = 112 / upem
k = 112 * 1.12 / 64
x0, base = 96, 236
parts.append(f'<g transform="translate({x0 - 3 * k:.2f} {base - 54 * k:.2f}) scale({k:.4f})">{mark_body(FULL, MADDER_LIGHT, LINEN)}</g>')
parts.append(f'<path transform="translate({x0 - 3 * k + 60.3 * k + 16:.2f} {base}) scale({s:.5f})" d="{d}" fill="{LINEN}"/>')
for i, line in enumerate(["Your machines have a day job.", "Tentmaker gives them a second one."]):
    parts.append(text_path(line, 54, BODY, LINEN, x0, 360 + i * 70, -0.01)[0])
parts.append(text_path("Self-hosted CI on your own Linux, macOS and Windows machines,", 27, SMALLTXT, MADDER_LIGHT, x0, 532)[0])
parts.append(text_path("pooled over Tailscale. Free and open source.", 27, SMALLTXT, MADDER_LIGHT, x0, 568)[0])
write("social.svg", svg("".join(parts), W, H, title="Tentmaker: self-hosted CI on your own machines"))
print("wrote", sorted(os.listdir(OUT)))
