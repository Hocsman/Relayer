#!/usr/bin/env python3
"""Build docs/social-preview.png, the card GitHub shows when the repository is
linked from X, Slack, Discord or Hacker News.

GitHub has no API for the social preview, so the image is uploaded by hand in
Settings -> General -> Social preview. This script exists so the asset can be
rebuilt when the interface changes rather than being a screenshot nobody can
reproduce.

    python3 docs/social-preview.py

Requires Pillow. The source screenshot is docs/gui-dashboard.png at 2880x1800;
the crop boxes below are in that coordinate space and have to be re-measured if
it is retaken at another size.
"""

import pathlib

from PIL import Image, ImageDraw, ImageFont

DOCS = pathlib.Path(__file__).resolve().parent
SOURCE = DOCS / "gui-dashboard.png"
TARGET = DOCS / "social-preview.png"

# GitHub renders the card small, so the type is large and the screenshot is
# reduced to the two things that carry the idea: an agent stopped, a decision
# waiting.
WIDTH, HEIGHT = 1280, 640
BACKGROUND = (10, 14, 20)
FOREGROUND = (237, 242, 249)
MUTED = (150, 164, 184)
ACCENT = (255, 138, 128)
FAINT = (110, 124, 144)
BORDER = (42, 52, 66)

FONTS = "/System/Library/Fonts/Supplemental"
TITLE = ImageFont.truetype(f"{FONTS}/Arial Bold.ttf", 78)
BODY = ImageFont.truetype(f"{FONTS}/Arial.ttf", 29)
LABEL = ImageFont.truetype(f"{FONTS}/Arial Bold.ttf", 18)
FOOTER = ImageFont.truetype(f"{FONTS}/Arial.ttf", 20)

TAGLINE = [
    "Run several AI coding agents",
    "at once, and answer their",
    "prompts from one place.",
]

# Agent pane header and the top of its terminal, then the pending decision card.
PANE_BOX = (29, 151, 1077, 720)
QUEUE_BOX = (2186, 281, 2866, 432)


def rounded(image, radius):
    mask = Image.new("L", image.size, 0)
    ImageDraw.Draw(mask).rounded_rectangle(
        [0, 0, image.size[0] - 1, image.size[1] - 1], radius=radius, fill=255
    )
    out = Image.new("RGBA", image.size, (0, 0, 0, 0))
    out.paste(image.convert("RGB"), (0, 0), mask)
    ImageDraw.Draw(out).rounded_rectangle(
        [0, 0, image.size[0] - 1, image.size[1] - 1],
        radius=radius,
        outline=BORDER + (255,),
        width=2,
    )
    return out


def build():
    card = Image.new("RGB", (WIDTH, HEIGHT), BACKGROUND)
    draw = ImageDraw.Draw(card)
    for y in range(HEIGHT):
        ratio = y / HEIGHT
        draw.line(
            [(0, y), (WIDTH, y)],
            fill=(int(10 + 8 * ratio), int(14 + 10 * ratio), int(20 + 14 * ratio)),
        )

    shot = Image.open(SOURCE)
    pane = shot.crop(PANE_BOX).resize((596, 324), Image.LANCZOS)
    queue = shot.crop(QUEUE_BOX).resize((596, 132), Image.LANCZOS)
    card.paste(rounded(pane, 14), (620, 80), rounded(pane, 14))
    card.paste(rounded(queue, 14), (620, 428), rounded(queue, 14))

    draw.text((72, 168), "Relayer", font=TITLE, fill=FOREGROUND)
    y = 272
    for line in TAGLINE:
        draw.text((72, y), line, font=BODY, fill=MUTED)
        y += 40
    draw.text((72, 432), "HUMAN IN THE LOOP", font=LABEL, fill=ACCENT)
    draw.text(
        (72, 462),
        "MIT · single Go binary · no API key, no proxy",
        font=FOOTER,
        fill=FAINT,
    )

    card.save(TARGET)
    print(f"wrote {TARGET} {card.size}")


if __name__ == "__main__":
    build()
