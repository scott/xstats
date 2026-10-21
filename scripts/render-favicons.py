#!/usr/bin/env python3
"""Rebuild the xstats favicon rasters.

Sources, next to this script:
  favicon/mark.svg     64px growth chart (outline #3d4a5c)
  favicon/mark-16.svg  16px drawing with a 1px stroke and pixel-aligned bars

Writes cmd/xstats/web/favicon.svg (the 16px drawing), the PNG sizes,
apple-touch-icon.png, and favicon.ico (16, 32, 48).

Requires rsvg-convert (package librsvg2-bin). No other libraries.
"""

import shutil
import struct
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "scripts" / "favicon"
OUT = ROOT / "cmd" / "xstats" / "web"
MARK = SRC / "mark.svg"
MARK16 = SRC / "mark-16.svg"

# Tile, bars, line, outline. Kept in sync with cmd/xstats/web/index.html.
TILE = "#07090d"
BARS = "#a371f7"
LINE = "#1d9bf0"
OUTLINE = "#3d4a5c"


def rsvg(src: Path, dest: Path, size: int, background: str | None = None) -> None:
    cmd = ["rsvg-convert", "-w", str(size), "-h", str(size)]
    if background:
        cmd += ["-b", background]
    cmd += ["-o", str(dest), str(src)]
    subprocess.check_call(cmd)


def png_size(blob: bytes) -> tuple[int, int]:
    sig = b"\x89PNG\r\n\x1a\n"
    if blob[:8] != sig or blob[12:16] != b"IHDR":
        raise SystemExit("rsvg-convert did not write a PNG")
    return struct.unpack(">II", blob[16:24])


def write_ico(dest: Path, pngs: list[bytes]) -> None:
    """PNG-compressed ICO. Width byte 0 means 256, which these sizes are not."""
    count = len(pngs)
    parts = [struct.pack("<HHH", 0, 1, count)]
    offset = 6 + 16 * count
    entries = []
    for blob in pngs:
        w, h = png_size(blob)
        if not 1 <= w < 256 or not 1 <= h < 256:
            raise SystemExit(f"ICO size {w}x{h} is out of range")
        entries.append(struct.pack("<BBBBHHII", w, h, 0, 0, 1, 32, len(blob), offset))
        offset += len(blob)
    dest.write_bytes(b"".join(parts + entries + pngs))


def expect(path: Path, size: int) -> bytes:
    blob = path.read_bytes()
    got = png_size(blob)
    if got != (size, size):
        raise SystemExit(f"{path.name}: expected {size}x{size}, got {got[0]}x{got[1]}")
    return blob


def main() -> None:
    rsvg_bin = shutil.which("rsvg-convert")
    if not rsvg_bin:
        sys.exit("rsvg-convert not found. Install librsvg2-bin and rerun.")
    for src in (MARK, MARK16):
        text = src.read_text()
        for color in (TILE, BARS, LINE, OUTLINE):
            if color not in text:
                sys.exit(f"{src.name} is missing {color}")
        # The xmlns declaration is the SVG namespace, not a fetched resource.
        if "href=" in text or "url(" in text or "https://" in text:
            sys.exit(f"{src.name} must not reference external files")

    OUT.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(MARK16, OUT / "favicon.svg")

    with tempfile.TemporaryDirectory() as tmp:
        tmp = Path(tmp)
        rsvg(MARK16, OUT / "favicon-16.png", 16)
        for size, name in (
            (32, "favicon-32.png"),
            (48, "favicon-48.png"),
            (192, "favicon-192.png"),
            (512, "favicon-512.png"),
        ):
            dest = OUT / name if size != 48 else tmp / name
            rsvg(MARK, dest, size)
        # Full-bleed tile. iOS masks the square, so the corners stay opaque.
        rsvg(MARK, OUT / "apple-touch-icon.png", 180, TILE)

        png16 = expect(OUT / "favicon-16.png", 16)
        png32 = expect(OUT / "favicon-32.png", 32)
        png48 = expect(tmp / "favicon-48.png", 48)
        expect(OUT / "favicon-192.png", 192)
        expect(OUT / "favicon-512.png", 512)
        expect(OUT / "apple-touch-icon.png", 180)
        write_ico(OUT / "favicon.ico", [png16, png32, png48])

    print(f"wrote favicons to {OUT}")


if __name__ == "__main__":
    main()
