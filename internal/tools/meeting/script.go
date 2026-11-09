package meeting

import (
	"encoding/json"
	"strings"
)

// bindingName is the page function the caption observer reports through
// (Runtime.addBinding).
const bindingName = "__habakkukCaptions"

// pageLib is the script every evaluation runs: one function of an
// operation and its argument. It reads the page and acts on it by labels
// and text, never by the obfuscated class names the platforms change often
// (except where captions offer nothing else).
//
//   - probe: {skip, captions} -> Probe
//   - find: {pattern} -> {x, y, label} of the first visible clickable element
//     whose label matches, scrolled into view, or null
//   - focus: {pattern} -> {value, label} of the first visible text field
//     whose label matches, focused with its text selected, or null
//   - captions: {captions, binding} -> "started" | "running": a
//     MutationObserver that reports caption blocks through the binding
const pageLib = `function(op, arg) {
  const norm = s => (s || '').replace(/[‘’]/g, "'").replace(/\s+/g, ' ').trim();
  const visible = el => {
    if (!el || !el.getClientRects || el.getClientRects().length === 0) return false;
    if (el.checkVisibility) return el.checkVisibility({opacityProperty: true, visibilityProperty: true});
    const st = getComputedStyle(el);
    return st.visibility !== 'hidden' && st.display !== 'none' && st.opacity !== '0';
  };
  const clickSel = 'button,[role="button"],a[href],input[type="button"],input[type="submit"],[role="menuitem"],[role="menuitemcheckbox"],[role="menuitemradio"],[role="switch"],[role="tab"],[role="option"],input[type="checkbox"]';
  const fieldSel = 'input:not([type]),input[type="text"],input[type="email"],input[type="search"],textarea,[contenteditable="true"],[contenteditable=""],[role="textbox"]';
  const label = el => {
    let l = norm(el.getAttribute('aria-label') || el.innerText || el.value || el.getAttribute('title') || '').slice(0, 120);
    const st = el.getAttribute('aria-pressed') || el.getAttribute('aria-checked') || (el.type === 'checkbox' ? String(el.checked) : null);
    if (st === 'true') l += ' [on]'; else if (st === 'false') l += ' [off]';
    return l;
  };
  const fieldLabel = el => {
    const lab = el.labels && el.labels.length ? el.labels[0].innerText : '';
    return [el.getAttribute('aria-label'), el.getAttribute('placeholder'), el.getAttribute('aria-placeholder'), lab,
      el.id, el.getAttribute('name'), el.getAttribute('data-tid')].filter(Boolean).map(norm).join(' | ').slice(0, 200);
  };
  const all = (sel, root) => { try { return Array.from((root || document).querySelectorAll(sel)); } catch (e) { return []; } };
  const containers = spec => {
    for (const sel of spec.containers) { const found = all(sel); if (found.length) return found; }
    return [];
  };
  const pick = (el, sels) => {
    for (const s of sels) { for (const f of all(s, el)) { const t = norm(f.innerText || f.textContent); if (t) return t; } }
    return '';
  };
  const items = spec => {
    const out = [];
    for (const c of containers(spec)) {
      let its = [];
      for (const s of spec.items) { its = all(s, c); if (its.length) break; }
      if (!its.length) its = Array.from(c.children);
      out.push(...its);
    }
    return out;
  };
  const extract = (it, spec) => {
    let speaker = pick(it, spec.speaker), text = pick(it, spec.text);
    if (!text) {
      const lines = (it.innerText || it.textContent || '').split('\n').map(norm).filter(Boolean);
      if (!speaker && lines.length >= 2 && lines[0].length <= 50) { speaker = lines[0]; text = lines.slice(1).join(' '); }
      else { text = lines.join(' '); if (speaker && text.startsWith(speaker + ' ')) text = text.slice(speaker.length + 1); }
    }
    return {speaker: norm(speaker), text: norm(text)};
  };
  if (op === 'probe') {
    let text = document.body ? (document.body.innerText || '') : '';
    for (const sel of arg.skip) for (const el of all(sel)) { const t = el.innerText; if (t && t.trim()) text = text.split(t).join('\n'); }
    const buttons = [], seen = new Set();
    for (const el of all(clickSel)) {
      if (!visible(el)) continue;
      const l = label(el);
      if (l && !seen.has(l)) { seen.add(l); buttons.push(l); }
      if (buttons.length >= 150) break;
    }
    const fields = [];
    for (const el of all(fieldSel)) { if (visible(el)) fields.push(fieldLabel(el)); if (fields.length >= 30) break; }
    return {url: location.href, title: document.title, text: norm(text).slice(0, 20000), buttons, fields,
      captions: containers(arg.captions).length > 0};
  }
  if (op === 'find') {
    const re = new RegExp(arg.pattern, 'i');
    for (const el of all(clickSel)) {
      if (!visible(el) || el.disabled || el.getAttribute('aria-disabled') === 'true') continue;
      const l = label(el);
      if (!re.test(l)) continue;
      el.scrollIntoView({block: 'center', inline: 'center'});
      const r = el.getBoundingClientRect();
      return {x: r.left + r.width / 2, y: r.top + r.height / 2, label: l};
    }
    return null;
  }
  if (op === 'focus') {
    const re = new RegExp(arg.pattern, 'i');
    for (const el of all(fieldSel)) {
      if (!visible(el) || el.disabled || el.readOnly) continue;
      const l = fieldLabel(el);
      if (!re.test(l)) continue;
      el.scrollIntoView({block: 'center'});
      el.focus();
      let value = '';
      if ('value' in el && el.tagName !== 'DIV') { value = el.value; el.select && el.select(); }
      else { value = norm(el.innerText); const r = document.createRange(); r.selectNodeContents(el); const s = getSelection(); s.removeAllRanges(); s.addRange(r); }
      return {value, label: l};
    }
    return null;
  }
  if (op === 'captions') {
    const st = window.__habakkukCaptionState;
    if (st && st.alive) return 'running';
    const spec = arg.captions, ids = new WeakMap(), last = new Map();
    const epoch = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
    let next = 1, timer = null;
    const scan = () => {
      timer = null;
      const batch = [], present = new Set();
      for (const it of items(spec)) {
        let id = ids.get(it);
        if (!id) { id = next++; ids.set(it, id); }
        present.add(id);
        const c = extract(it, spec);
        if (!c.text) continue;
        const key = c.speaker + '\u0000' + c.text;
        if (last.get(id) === key) continue;
        last.set(id, key);
        batch.push({id, s: c.speaker, t: c.text});
      }
      for (const id of Array.from(last.keys())) if (!present.has(id)) { last.delete(id); batch.push({id, g: true}); }
      if (batch.length && typeof window[arg.binding] === 'function') {
        try { window[arg.binding](JSON.stringify({e: epoch, c: batch})); } catch (e) {}
      }
    };
    const kick = () => { if (!timer) timer = setTimeout(scan, 250); };
    const obs = new MutationObserver(kick);
    obs.observe(document.documentElement, {subtree: true, childList: true, characterData: true});
    const iv = setInterval(scan, 2000);
    window.__habakkukCaptionState = {alive: true, stop() { obs.disconnect(); clearInterval(iv); this.alive = false; }};
    scan();
    return 'started';
  }
  throw new Error('unknown operation ' + op);
}`

// pageCall is the expression for one operation. It starts with a comment
// naming the operation and its argument, which the test driver reads.
func pageCall(op string, arg any) string {
	a, _ := json.Marshal(arg)
	o, _ := json.Marshal(op)
	header, _ := json.Marshal(map[string]any{"op": op, "arg": json.RawMessage(a)})
	var b strings.Builder
	b.WriteString("/*")
	b.WriteString(strings.ReplaceAll(string(header), "*/", `*\/`))
	b.WriteString("*/(")
	b.WriteString(pageLib)
	b.WriteString(")(")
	b.Write(o)
	b.WriteString(", ")
	b.Write(a)
	b.WriteString(")")
	return b.String()
}

// captionBatch is what the observer sends: the page's epoch (blocks are
// numbered anew in every page load) and the changed blocks.
type captionBatch struct {
	Epoch   string `json:"e"`
	Changes []struct {
		ID      int    `json:"id"`
		Speaker string `json:"s"`
		Text    string `json:"t"`
		Gone    bool   `json:"g"`
	} `json:"c"`
}
