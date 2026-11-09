# Habakkuk brand files

The mark is two tablets with rounded tops that form an H, with the watchman's light above them, from Habakkuk 2:1-2: "I will stand upon my watch ... Write the vision, and make it plain upon tables." The wordmark is drawn from monoline strokes, so no font is needed.

These files are trademarks of Ayobami Joseph and are **not** covered by the project's Apache License 2.0. Read [TRADEMARKS.md](../../TRADEMARKS.md) before using them. More guidance is in [docs/contributing/brand.md](../../docs/contributing/brand.md).

## Files

| File | What it is | Use it on |
|---|---|---|
| `logo.svg` | Mark and wordmark, ink with the gold light | Light backgrounds |
| `logo-dark.svg` | Mark and wordmark, cream with the gold light | Dark backgrounds |
| `wordmark.svg`, `wordmark-dark.svg` | The word "habakkuk" alone, ink or cream | Where the mark is already shown nearby |
| `mark.svg`, `mark-dark.svg` | The mark alone, ink or cream, with the gold light | Small spaces, avatars |
| `app-icon.svg`, `app-icon-1024.png` | Cream mark on an ink rounded square | App icons, store listings |
| `favicon.svg`, `favicon.ico`, `favicon-16.png`, `favicon-32.png`, `favicon-180.png`, `favicon-192.png`, `favicon-512.png` | The mark at favicon and touch-icon sizes | Websites, PWA manifests |
| `social-card.png` | 1200 x 630 preview image | Link previews (Open Graph, X) |
| `generate.py` | Draws the SVG lockups from code (`python3 generate.py` writes `out/`) | Regenerating or adjusting the SVGs |

## Colours

| Name | Hex | Role |
|---|---|---|
| Ink | `#0E1116` | The mark and text on light backgrounds; the app icon background |
| Cream | `#F4F1EA` | The mark and text on dark backgrounds |
| Gold | `#FFB020` | The light above the tablets, and only that |

## Do

- Use `logo.svg` on light backgrounds and `logo-dark.svg` on dark ones; in Markdown, a `<picture>` with `prefers-color-scheme` picks the right one.
- Keep clear space around the mark of at least the width of one tablet.
- Scale the files proportionally, and prefer the SVGs over the PNGs.
- Use the mark alone when the space is too small for the wordmark to be read.

## Don't

- Recolour the mark or the light, add gradients, shadows or outlines, or put the gold light anywhere else.
- Stretch, rotate, crop or rearrange the tablets and the light, or redraw the wordmark in a font.
- Put the ink logo on a dark background or the cream logo on a light one.
- Use the name or these files for a fork or a product of your own: see [TRADEMARKS.md](../../TRADEMARKS.md).
