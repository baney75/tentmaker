#!/bin/sh
# Render brand/export/ and brand/brand-sheet.png from the SVGs gen.py wrote.
# Needs rsvg-convert (librsvg) and ImageMagick 7.
set -eu
cd "$(dirname "$0")/.."
mkdir -p export
for s in 16 32 48; do rsvg-convert -w $s -h $s favicon.svg -o export/favicon-$s.png; done
magick export/favicon-16.png export/favicon-32.png export/favicon-48.png export/favicon.ico
for s in 180 192 512 1024; do rsvg-convert -w $s -h $s icon.svg -o export/icon-$s.png; done
rsvg-convert -w 1280 -h 640 social.svg -o export/social.png
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
rsvg-convert -h 110 wordmark.svg -o "$t/w.png"
rsvg-convert -h 110 wordmark-on-dark.svg -o "$t/wd.png"
rsvg-convert -w 150 -h 150 icon.svg -o "$t/i.png"
rsvg-convert -w 64 -h 64 mark.svg -o "$t/m64.png"
rsvg-convert -w 32 -h 32 mark.svg -o "$t/m32.png"
rsvg-convert -w 24 -h 24 mark-small.svg -o "$t/m24.png"
rsvg-convert -w 32 -h 32 favicon.svg -o "$t/f32.png"
rsvg-convert -w 16 -h 16 favicon.svg -o "$t/f16.png"
magick \
  \( "$t/w.png" -background '#F3EEE3' -gravity center -extent 700x230 \) \
  \( "$t/wd.png" -background '#1F1C19' -gravity center -extent 700x230 \) +append "$t/top.png"
magick -background '#F3EEE3' \
  \( "$t/i.png" -gravity center -extent 220x210 \) \
  \( "$t/m64.png" -gravity center -extent 150x210 \) \
  \( "$t/m32.png" -gravity center -extent 110x210 \) \
  \( "$t/m24.png" -gravity center -extent 110x210 \) \
  \( "$t/f32.png" -gravity center -extent 110x210 \) \
  \( "$t/f16.png" -gravity center -extent 110x210 \) +append -gravity west -extent 1400x210 "$t/bottom.png"
magick "$t/top.png" "$t/bottom.png" -append brand-sheet.png
echo "wrote export/ and brand-sheet.png"
