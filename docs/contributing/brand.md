---
title: Brand
description: "Which Habakkuk logo file to use, the colours (ink, cream and gold), the rules for using them, and what the mark means."
order: 2
---

Here's which Habakkuk logo file to use, what the colours are, and the rules that go with them. The brand files live in [`assets/brand/`](../../assets/brand/), and a [README](../../assets/brand/README.md) there lists each one. The name and the files are trademarks, and the Apache License 2.0 doesn't cover them. See [TRADEMARKS.md](../../TRADEMARKS.md).

## The meaning

The mark is two tablets with rounded tops that form an H, with the watchman's light above them. It comes from Habakkuk 2:1-2. There, the prophet stands on his watch to see what will be said to him, and he's told to write the vision and make it plain upon tablets. The tablets are the plain writing. The light is the watch kept above them.

## Colours

| Name | Hex | Role |
|---|---|---|
| Ink | `#0E1116` | The mark and text on light backgrounds; the app icon background |
| Cream | `#F4F1EA` | The mark and text on dark backgrounds |
| Gold | `#FFB020` | The light above the tablets, and only that |

## Which file to use

- **Logo** (mark and wordmark): use `logo.svg` on light backgrounds and `logo-dark.svg` on dark ones. In Markdown that GitHub renders, let the reader's theme choose:

  ```html
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/logo-dark.svg">
    <img src="assets/brand/logo.svg" alt="Habakkuk" height="72">
  </picture>
  ```

- **Mark only**: use `mark.svg` or `mark-dark.svg` for small spaces and avatars.
- **Wordmark only**: use `wordmark.svg` or `wordmark-dark.svg` when the mark is already nearby.
- **App icon**: use `app-icon.svg` or `app-icon-1024.png` (a cream mark on an ink square).
- **Favicons**: use `favicon.svg`, `favicon.ico` and the PNGs at 16, 32, 180, 192 and 512 pixels.
- **Link previews**: use `social-card.png` (1200 x 630).

The wordmark is drawn from strokes, so you don't need a font file. `generate.py` draws the SVG lockups from code. If you need to change them, change `generate.py` instead of editing the SVGs by hand. Then copy the files you need from its `out/` folder.

## Rules

- Keep the colours as they are. Gold is for the light and nothing else.
- Don't stretch, rotate, recolour or redraw the mark or the wordmark. Keep some clear space around them.
- Forks and products built on Habakkuk use their own name and logo. Plain-text statements such as "works with Habakkuk" are fine. See [TRADEMARKS.md](../../TRADEMARKS.md).
