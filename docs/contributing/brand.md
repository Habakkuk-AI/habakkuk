---
title: Brand
description: "How to use the Habakkuk logo files, the colours (ink, cream and gold), and what the mark means."
order: 2
---

The brand files live in [`assets/brand/`](../../assets/brand/), with a [README](../../assets/brand/README.md) listing each one. The name and the files are trademarks and are not covered by the Apache License 2.0: see [TRADEMARKS.md](../../TRADEMARKS.md).

## The meaning

The mark is two tablets with rounded tops that form an H, with the watchman's light above them. It comes from Habakkuk 2:1-2: the prophet stands on his watch to see what will be said to him, and is told to write the vision and make it plain upon tablets. The tablets are the plain writing; the light is the watch kept above them.

## Colours

| Name | Hex | Role |
|---|---|---|
| Ink | `#0E1116` | The mark and text on light backgrounds; the app icon background |
| Cream | `#F4F1EA` | The mark and text on dark backgrounds |
| Gold | `#FFB020` | The light above the tablets, and only that |

## Which file to use

- **Logo** (mark and wordmark): `logo.svg` on light backgrounds, `logo-dark.svg` on dark ones. In Markdown that GitHub renders, let the reader's theme choose:

  ```html
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/logo-dark.svg">
    <img src="assets/brand/logo.svg" alt="Habakkuk" height="72">
  </picture>
  ```

- **Mark only**: `mark.svg` or `mark-dark.svg`, for small spaces and avatars.
- **Wordmark only**: `wordmark.svg` or `wordmark-dark.svg`, when the mark is already nearby.
- **App icon**: `app-icon.svg` or `app-icon-1024.png` (cream mark on an ink square).
- **Favicons**: `favicon.svg`, `favicon.ico` and the PNGs at 16, 32, 180, 192 and 512 pixels.
- **Link previews**: `social-card.png` (1200 x 630).

The wordmark is drawn from strokes, so no font file is needed. `generate.py` draws the SVG lockups from code; change it rather than editing the SVGs by hand, then copy the files you need from its `out/` folder.

## Rules

- Keep the colours as they are: the gold is only the light.
- Do not stretch, rotate, recolour or redraw the mark or the wordmark, and keep some clear space around them.
- Forks and products built on Habakkuk use their own name and logo; plain-text statements such as "works with Habakkuk" are fine. See [TRADEMARKS.md](../../TRADEMARKS.md).
