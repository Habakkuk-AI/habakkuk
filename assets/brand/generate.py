"""Generates the Habakkuk logo files in this folder.

The mark is two tablets with rounded tops that form an H, with a light
above them. The wordmark is drawn from monoline strokes, so no font is
needed. Run: python3 generate.py (writes out/; copy the files you need).
"""
S=13.0; h=S/2; r=16.5; base=100; xh=56; asc=30; cy=78; gap=8
def circ(cx): return (f"M{cx+r} {cy} A{r} {r} 0 1 0 {cx-r} {cy} A{r} {r} 0 1 0 {cx+r} {cy} Z","round")
def glyph(c,x):
    L=x+h
    if c=='h': return [(f"M{L} {asc} V{base}","round"),(f"M{L} {cy} A{r} {r} 0 0 1 {L+2*r} {cy}","butt"),(f"M{L+2*r} {cy} V{base}","round")], 2*r+S
    if c=='a': cx=x+h+r; return [circ(cx),(f"M{cx+r} {xh} V{base}","round")], 2*r+S
    if c=='b': cx=L+r; return [(f"M{L} {asc} V{base}","round"),circ(cx)], 2*r+S
    if c=='k': return [(f"M{L} {asc} V{base}","round"),(f"M{L+28} {xh} L{L+4} {cy+1}","round"),(f"M{L+10} {cy-2} L{L+29} {base}","round")], 29+S
    if c=='u': return [(f"M{L} {xh} V{cy}","round"),(f"M{L} {cy} A{r} {r} 0 0 0 {L+2*r} {cy}","butt"),(f"M{L+2*r} {xh} V{base}","round")], 2*r+S
def word(text):
    x=0; ps=[]
    for c in text:
        p,w=glyph(c,x); ps+=p; x+=w+gap
    return ps, x-gap
MARK='<g fill="{ink}"><path d="M26 108 V50 A15 15 0 0 1 56 50 V108 a4 4 0 0 1 -4 4 H30 a4 4 0 0 1 -4 -4 Z"/><path d="M72 108 V50 A15 15 0 0 1 102 50 V108 a4 4 0 0 1 -4 4 H76 a4 4 0 0 1 -4 -4 Z"/><rect x="54" y="70" width="20" height="14"/></g><circle cx="64" cy="24" r="9" fill="{gold}"/>'
def wordsvg(ink):
    ps,w=word("habakkuk")
    return "".join(f'<path d="{d}" stroke-linecap="{cap}"/>' for d,cap in ps), w
def lockup(ink="#0E1116", gold="#FFB020", bg=None):
    mw=124; g,w=wordsvg(ink); W=mw-6+w+12+12
    s=24.5-35*mw/128
    bgr=f'<rect x="-12" y="0" width="{W}" height="118" fill="{bg}"/>' if bg else ""
    return f'''<svg xmlns="http://www.w3.org/2000/svg" viewBox="-12 0 {W:.1f} 118" width="{W*2:.0f}" height="236">{bgr}<g transform="translate(-8,{s:.2f}) scale({mw/128:.4f})">{MARK.format(ink=ink,gold=gold)}</g><g transform="translate({mw-6},0)" fill="none" stroke="{ink}" stroke-width="{S}" stroke-linejoin="round">{g}</g></svg>'''
def wordmark(ink="#0E1116"):
    g,w=wordsvg(ink)
    return f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="-8 22 {w+16:.1f} 86" width="{(w+16)*2:.0f}" height="172"><g fill="none" stroke="{ink}" stroke-width="{S}" stroke-linejoin="round">{g}</g></svg>'
def mark(ink="#0E1116", gold="#FFB020", bg=None, rx=28, pad=0):
    bgr=f'<rect width="128" height="128" rx="{rx}" fill="{bg}"/>' if bg else ""
    inner=MARK.format(ink=ink,gold=gold)
    if pad: inner=f'<g transform="translate({pad},{pad}) scale({(128-2*pad)/128:.4f})">{inner}</g>'
    return f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128" width="512" height="512">{bgr}{inner}</svg>'
if __name__=="__main__":
    import os
    os.makedirs("out",exist_ok=True)
    files={
     "logo.svg":lockup(), "logo-dark.svg":lockup(ink="#F4F1EA"),
     "wordmark.svg":wordmark(), "wordmark-dark.svg":wordmark("#F4F1EA"),
     "mark.svg":mark(), "mark-dark.svg":mark(ink="#F4F1EA"),
     "app-icon.svg":mark(ink="#F4F1EA",bg="#0E1116",pad=18),
     "favicon.svg":mark(ink="#0E1116",bg="#FFFFFF",rx=24,pad=8),
    }
    for n,s in files.items(): open("out/"+n,"w").write(s)
    open("preview.html","w").write('''<html><body style="margin:0;font-family:sans-serif">
<div style="background:#fff;padding:40px"><img src="out/logo.svg" style="height:120px"></div>
<div style="background:#0E1116;padding:40px"><img src="out/logo-dark.svg" style="height:120px"></div>
<div style="background:#F4F1EA;padding:30px;display:flex;gap:40px;align-items:center"><img src="out/app-icon.svg" style="height:160px"><img src="out/favicon.svg" style="height:64px;border:1px solid #ddd;border-radius:14px"><img src="out/app-icon.svg" style="height:32px"><img src="out/mark.svg" style="height:160px"></div></body></html>''')
