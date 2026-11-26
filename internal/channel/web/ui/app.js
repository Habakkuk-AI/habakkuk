"use strict";
// Habakkuk's control dashboard: a small single-page app over the JSON API.
// No build step and no libraries. Everything that comes from the server is
// put into the page as text, or through md(), which escapes first.

const $ = id => document.getElementById(id);
const store = {
  get(k, d) { try { const v = localStorage.getItem("habakkuk." + k); return v === null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem("habakkuk." + k, JSON.stringify(v)); } catch {} },
};
let token = store.get("token", "");
let app = null; // GET /api/app: agents, default, speech, transcription
let cleanups = [];

// DOM

function h(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "class") e.className = v;
    else if (k === "value" || k === "checked" || k === "selected" || k === "disabled") e[k] = v;
    else e.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    e.append(kid instanceof Node ? kid : String(kid));
  }
  return e;
}
const SVGNS = "http://www.w3.org/2000/svg";
function s(tag, attrs, ...kids) {
  const e = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, v);
  for (const kid of kids.flat()) if (kid != null) e.append(kid instanceof Node ? kid : String(kid));
  return e;
}
function icon(paths) {
  return s("svg", { viewBox: "0 0 24 24", "aria-hidden": "true" }, paths.map(d => s("path", { d })));
}
const ICONS = {
  mic: ["M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3z", "M19 11a7 7 0 0 1-14 0", "M12 18v3"],
  clip: ["M21 11.5l-8.5 8.5a5 5 0 0 1-7-7l9-9a3.5 3.5 0 0 1 5 5l-9 9a2 2 0 0 1-3-3l8-8"],
};
function esc(t) {
  return String(t).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}
// Basic Markdown: fenced code, inline code, bold, headings, bullets and
// links. Everything is escaped first, so text can never inject markup.
function md(src) {
  const div = h("div");
  div.innerHTML = String(src || "").split("```").map((part, i) => {
    if (i % 2) {
      const nl = part.indexOf("\n");
      const code = nl >= 0 && /^[\w+#.-]*$/.test(part.slice(0, nl)) ? part.slice(nl + 1) : part;
      return "<pre><code>" + esc(code.replace(/\n$/, "")) + "</code></pre>";
    }
    return inline(part);
  }).join("");
  return div;
}
function inline(t) {
  const codes = [];
  t = esc(t).replace(/`([^`\n]+)`/g, (_, c) => "\u0000" + (codes.push(c) - 1) + "\u0000");
  t = t.replace(/^#{1,6} +(.+)$/gm, "<strong>$1</strong>");
  t = t.replace(/^[-*] +/gm, "&bull; ");
  t = t.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
  t = t.replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
  t = t.replace(/(^|[\s(])(https?:\/\/[^\s<]*[^\s<.,;:!?)])/g, '$1<a href="$2" target="_blank" rel="noopener noreferrer">$2</a>');
  t = t.replace(/\u0000(\d+)\u0000/g, (_, i) => "<code>" + codes[i] + "</code>");
  return t.replace(/\n/g, "<br>");
}
function rid() {
  const b = new Uint8Array(8);
  crypto.getRandomValues(b);
  return Array.from(b, x => x.toString(16).padStart(2, "0")).join("");
}

// Formatting

function when(t) {
  if (!t) return "";
  const d = new Date(t);
  if (isNaN(d) || d.getTime() <= 0) return "";
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}
function ago(t) {
  if (!t) return "";
  const sec = Math.round((Date.now() - new Date(t)) / 1000);
  if (isNaN(sec)) return "";
  const fut = sec < 0, a = Math.abs(sec);
  const v = a < 60 ? a + "s" : a < 3600 ? Math.round(a / 60) + "m" : a < 86400 ? Math.round(a / 3600) + "h" : Math.round(a / 86400) + "d";
  return fut ? "in " + v : v + " ago";
}
function tokens(n) {
  n = n || 0;
  if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
  if (n >= 1e4) return Math.round(n / 1e3) + "k";
  if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, "") + "k";
  return String(n);
}
function money(n) { return !n ? "$0" : "$" + n.toFixed(n >= 100 ? 0 : n >= 0.01 ? 2 : 4); }
function badge(text, kind) { return h("span", { class: "badge " + (kind || "") }, text); }
function stateKind(st) {
  return { connected: "ok", done: "ok", todo: "info", doing: "warn", running: "warn", blocked: "bad", failed: "bad",
    backoff: "bad", stopped: "bad", cancelled: "", canceled: "" }[st] ?? "";
}

// API

async function api(path, opts = {}) {
  const headers = {};
  if (token) headers.Authorization = "Bearer " + token;
  let body;
  const method = opts.method || "GET";
  if (opts.form) {
    body = opts.form;
    headers["X-Habakkuk-Client"] = "dashboard";
  } else if (method === "POST" || method === "PUT") {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body ?? {});
  }
  const res = await fetch("../api/" + path, { method, headers, body, signal: opts.signal });
  if (res.status === 401) { gate(); throw new Error("the token is missing or wrong"); }
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    throw new Error(j.error || "HTTP " + res.status);
  }
  if (opts.raw) return res;
  if (res.status === 204) return null;
  return res.json();
}

// stream follows a Server-Sent Events endpoint over fetch (so the token
// travels in a header), reconnecting until the signal aborts. path() is
// called on every connect, so it can carry a resume position.
async function stream(path, onEvent, signal, onState) {
  while (!signal.aborted) {
    try {
      const headers = token ? { Authorization: "Bearer " + token } : {};
      const res = await fetch("../api/" + path(), { headers, signal });
      if (res.status === 401) { onState?.(false); gate(); return; }
      if (!res.ok) throw new Error("HTTP " + res.status);
      onState?.(true);
      const reader = res.body.getReader(), dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          const block = buf.slice(0, i);
          buf = buf.slice(i + 2);
          let event = "message", data = "";
          for (const line of block.split("\n")) {
            if (line.startsWith("event:")) event = line.slice(6).trim();
            else if (line.startsWith("data:")) data += line.slice(5).trim();
          }
          if (!signal.aborted && data) {
            try { onEvent(event, JSON.parse(data)); } catch (err) { console.error(err); }
          }
        }
      }
    } catch (err) {
      if (signal.aborted) return;
    }
    onState?.(false);
    await sleep(2000);
  }
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

function toast(msg, bad) {
  const t = h("div", { class: "toast" + (bad ? " bad" : "") }, msg);
  $("toasts").append(t);
  setTimeout(() => t.remove(), bad ? 6000 : 3000);
}
const fail = err => toast(err.message || String(err), true);

function modal(...content) {
  const d = $("modal");
  const body = $("modalBody");
  body.textContent = "";
  body.append(...content.filter(c => c != null && c !== false));
  if (!d.open) d.showModal();
  return d;
}
function closeModal() { if ($("modal").open) $("modal").close(); }
$("modal").addEventListener("click", e => { if (e.target === $("modal")) closeModal(); });

function gate() {
  const g = $("gate");
  $("tok").value = token;
  if (!g.open) g.showModal();
  $("tok").focus();
}
$("gateForm").addEventListener("submit", () => {
  token = $("tok").value.trim();
  store.set("token", token);
  boot();
});
$("tokenBtn").addEventListener("click", gate);

// Theme: auto (the system's), light or dark.
function applyTheme() {
  const t = store.get("theme", "dark");
  if (t === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", t);
  $("themeBtn").textContent = "Theme: " + t;
}
$("themeBtn").addEventListener("click", () => {
  const order = ["auto", "light", "dark"];
  store.set("theme", order[(order.indexOf(store.get("theme", "dark")) + 1) % 3]);
  applyTheme();
});

// Navigation

function navOpen(on) { $("nav").classList.toggle("open", on); $("scrim").classList.toggle("show", on); }
$("menuBtn").addEventListener("click", () => navOpen(!$("nav").classList.contains("open")));
$("scrim").addEventListener("click", () => navOpen(false));
$("nav").addEventListener("click", e => { if (e.target.closest("a")) navOpen(false); });

const pages = {};
const PAGE_TITLES = { routing: "Who answers where", config: "Settings", overview: "Status" };
function onLeave(fn) { cleanups.push(fn); }
function every(ms, fn) {
  fn();
  const id = setInterval(() => { if (!document.hidden) fn(); }, ms);
  onLeave(() => clearInterval(id));
}
function route() {
  for (const fn of cleanups.splice(0)) { try { fn(); } catch {} }
  closeModal();
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  const name = pages[parts[0]] ? parts[0] : "home";
  for (const a of document.querySelectorAll("nav a")) a.classList.toggle("on", a.dataset.page === name);
  const view = $("view");
  view.className = "";
  view.textContent = "";
  view.scrollTop = 0;
  $("title").textContent = PAGE_TITLES[name] || name[0].toUpperCase() + name.slice(1);
  pages[name](view, parts.slice(1));
}
window.addEventListener("hashchange", route);
function go(hash) { if (location.hash === hash) route(); else location.hash = hash; }

function agentSelect(value, opts = {}) {
  const sel = h("select", { "aria-label": "Agent" });
  if (opts.any) sel.append(h("option", { value: "" }, opts.any));
  for (const a of app?.agents || []) sel.append(h("option", { value: a, selected: a === value }, a));
  if (value != null) sel.value = value;
  return sel;
}
function field(label, input) { return h("label", { class: "field" }, label, input); }
function table(head, rows) {
  return h("div", { class: "tablewrap" }, h("table", {},
    h("thead", {}, h("tr", {}, head.map(c => typeof c === "string" ? h("th", {}, c) : c))),
    h("tbody", {}, rows)));
}
function loading(view) { view.textContent = ""; view.append(h("div", { class: "empty" }, "Loading…")); }

// First-run guidance. Every empty page says what belongs there and offers
// the one step that fills it; most steps are a message to the agent, so
// they open Chat with that message ready to send.
const STARTERS = [
  { label: "Get a morning brief", text: "Every weekday at 8, send me a short brief of my day." },
  { label: "Teach it about you", text: "Remember that I like short, direct answers." },
  { label: "See what it can do", text: "What can you do for me? Give me three ideas I can try right now." },
];
let draft = ""; // a message Chat puts in its box when it opens
function ask(text) { draft = text; go("#/chat"); }
function emptyState(text, ...actions) {
  return h("div", { class: "empty" }, h("div", {}, text),
    actions.length ? h("div", { class: "row starters" }, actions) : null);
}
function askBtn(label, text) {
  return h("button", { type: "button", class: "btn small", title: text, onclick: () => ask(text) }, label);
}

// Overview

pages.overview = view => {
  every(5000, async () => {
    let o;
    try { o = await api("overview"); } catch (err) { view.textContent = ""; view.append(h("div", { class: "empty err" }, err.message)); return; }
    const stat = (n, l, href) => h(href ? "a" : "div", { class: "panel stat", href }, h("div", { class: "n" }, n), h("div", { class: "l" }, l));
    view.textContent = "";
    // First-run pointers: gone once the install is in use (the agent has
    // answered, or there is a schedule or a second channel), or hidden by hand.
    if (!o.established && !store.get("startedHidden", false)) {
      view.append(h("div", { class: "panel start" },
        h("div", { class: "row" }, h("h2", { class: "grow" }, "Get started"),
          h("button", { type: "button", class: "link small", onclick: () => { store.set("startedHidden", true); view.querySelector(".start")?.remove(); } }, "Hide")),
        h("p", { class: "muted small" }, "Your agent is ready. Each of these takes one message:"),
        h("ol", { class: "steps" },
          h("li", {}, h("a", { href: "#/chat" }, "Say hi in Chat"), ". The first chat is a short welcome where you name your agent and say how you like to work."),
          h("li", {}, h("button", { type: "button", class: "link", onclick: () => ask(STARTERS[0].text) }, STARTERS[0].label),
            ". It runs every weekday at 8 and shows up under Schedules."),
          h("li", {}, h("button", { type: "button", class: "link", onclick: () => ask(STARTERS[1].text) }, STARTERS[1].label),
            ". It keeps that in Memory and uses it from then on.")),
        h("p", { class: "muted small" }, "To talk from your phone, run ", h("code", {}, "hk setup channel"), " in a terminal: WhatsApp, Telegram, Slack and more.")));
    }
    view.append(h("div", { class: "grid" },
      stat(o.queue.pending, "jobs waiting"),
      stat(o.queue.running, "jobs running"),
      stat(o.approvals, "approvals waiting", "#/approvals"),
      o.open_cards != null && stat(o.open_cards, "open cards", "#/board"),
      o.schedules && stat(o.schedules.enabled + " / " + o.schedules.total, "schedules on", "#/schedules"),
    ));
    view.append(h("div", { class: "panel" }, h("h2", {}, "Channels"),
      o.channels.length ? table(["Account", "Type", "State", h("th", { class: "hide-sm" }, "Since"), "Restarts", "Last error"],
        o.channels.map(c => h("tr", {},
          h("td", {}, c.account), h("td", {}, c.type), h("td", {}, badge(c.state, stateKind(c.state))),
          h("td", { class: "hide-sm", title: when(c.since) }, ago(c.since)),
          h("td", { class: "num" }, c.restarts), h("td", { class: "small err" }, c.last_error || ""))))
        : emptyState("No channels yet. Run hk setup channel in a terminal to talk from WhatsApp, Telegram, Slack and more.")));
    view.append(h("div", { class: "panel" }, h("h2", {}, "Running now"),
      o.running.length ? table(["Kind", "Agent", "Job", "Started"],
        o.running.map(j => h("tr", j.session ? { class: "click", onclick: () => go("#/sessions/" + j.session) } : {},
          h("td", {}, j.kind), h("td", {}, j.agent), h("td", { class: "mono small" }, j.id),
          h("td", { title: when(j.updated_at) }, ago(j.updated_at)))))
        : h("div", { class: "empty" }, "Nothing is running.")));
    view.append(h("div", { class: "panel" }, h("h2", {}, "Recent errors"),
      o.errors.length ? h("ul", { class: "list" }, o.errors.map(j => h("li", j.session ? { class: "click", onclick: () => go("#/sessions/" + j.session) } : {},
        h("div", { class: "row" }, badge(j.status, stateKind(j.status)), h("strong", {}, j.kind), h("span", { class: "muted" }, j.agent),
          h("span", { class: "grow" }), h("span", { class: "muted small", title: when(j.updated_at) }, ago(j.updated_at))),
        h("div", { class: "small err" }, j.error))))
        : h("div", { class: "empty" }, "No errors in the last week.")));
  });
};

// Chat

const speaker = { queue: [], playing: false, el: null, conv: "" };
// A short silent clip, played inside a tap, lets the browser play later
// replies without one (mobile browsers block audio started outside a tap).
function silentWav() {
  const n = 800, buf = new ArrayBuffer(44 + n * 2), v = new DataView(buf);
  const str = (o, t) => [...t].forEach((c, i) => v.setUint8(o + i, c.charCodeAt(0)));
  str(0, "RIFF"); v.setUint32(4, 36 + n * 2, true); str(8, "WAVEfmt "); v.setUint32(16, 16, true);
  v.setUint16(20, 1, true); v.setUint16(22, 1, true); v.setUint32(24, 8000, true); v.setUint32(28, 16000, true);
  v.setUint16(32, 2, true); v.setUint16(34, 16, true); str(36, "data"); v.setUint32(40, n * 2, true);
  return URL.createObjectURL(new Blob([buf], { type: "audio/wav" }));
}
function unlockAudio() {
  if (speaker.el) return;
  speaker.el = new Audio();
  const url = silentWav();
  speaker.el.src = url;
  speaker.el.play().catch(() => {}).finally(() => URL.revokeObjectURL(url));
}
function speak(conv, seq) {
  speaker.conv = conv;
  speaker.queue.push(seq);
  if (!speaker.playing) playNext();
}
function stopSpeaking() {
  speaker.queue = [];
  if (speaker.el) speaker.el.pause();
  speaker.playing = false;
}
async function playNext() {
  const seq = speaker.queue.shift();
  if (seq == null) { speaker.playing = false; return; }
  speaker.playing = true;
  try {
    const res = await api(`chat/${encodeURIComponent(speaker.conv)}/messages/${seq}/speech`, { raw: true });
    const url = URL.createObjectURL(await res.blob());
    const el = speaker.el || (speaker.el = new Audio());
    await new Promise(resolve => {
      el.onended = el.onerror = resolve;
      el.src = url;
      el.play().catch(resolve);
    });
    URL.revokeObjectURL(url);
  } catch (err) { fail(err); }
  playNext();
}

// The microphone: one stream, a recorder per utterance, and a level meter
// for hands-free mode.
const mic = { stream: null, mr: null, chunks: [], mime: "", ctx: null, analyser: null, buf: null };
async function micStream() {
  if (!mic.stream) {
    if (!navigator.mediaDevices?.getUserMedia) throw new Error("this browser cannot record (the page needs https or localhost)");
    mic.stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true } });
  }
  return mic.stream;
}
function micRelease() {
  if (mic.mr && mic.mr.state !== "inactive") mic.mr.stop();
  mic.stream?.getTracks().forEach(t => t.stop());
  mic.ctx?.close().catch(() => {});
  Object.assign(mic, { stream: null, mr: null, ctx: null, analyser: null });
}
function recMime() {
  for (const t of ["audio/webm;codecs=opus", "audio/webm", "audio/mp4", "audio/ogg;codecs=opus"]) {
    if (window.MediaRecorder?.isTypeSupported?.(t)) return t;
  }
  return "";
}
async function recStart() {
  const st = await micStream();
  mic.mime = recMime();
  mic.chunks = [];
  mic.mr = new MediaRecorder(st, mic.mime ? { mimeType: mic.mime } : undefined);
  mic.mr.ondataavailable = e => { if (e.data.size) mic.chunks.push(e.data); };
  mic.mr.start(250);
  mic.started = Date.now();
}
function recStop() {
  return new Promise(resolve => {
    const mr = mic.mr;
    if (!mr || mr.state === "inactive") { resolve(null); return; }
    mr.onstop = () => {
      const type = mr.mimeType || mic.mime || "audio/webm";
      const ext = type.includes("mp4") ? ".m4a" : type.includes("ogg") ? ".ogg" : ".webm";
      resolve({ blob: new Blob(mic.chunks, { type }), name: "voice" + ext, ms: Date.now() - mic.started });
    };
    mr.stop();
  });
}
const recording = () => mic.mr && mic.mr.state === "recording";
function level() {
  if (!mic.analyser) {
    mic.ctx = new (window.AudioContext || window.webkitAudioContext)();
    mic.analyser = mic.ctx.createAnalyser();
    mic.analyser.fftSize = 1024;
    mic.ctx.createMediaStreamSource(mic.stream).connect(mic.analyser);
    mic.buf = new Float32Array(mic.analyser.fftSize);
  }
  mic.analyser.getFloatTimeDomainData(mic.buf);
  let sum = 0;
  for (const x of mic.buf) sum += x * x;
  return Math.sqrt(sum / mic.buf.length);
}

pages.chat = (view, [want]) => {
  view.className = "flush";
  let convs = store.get("convs", []);
  let current = want || store.get("current", "");
  let lastSeq = 0, openedAt = 0, ctrl = null, typingTimer = 0;
  let files = [];
  let act = null; // the running turn's activity: { group, cards, draft, text }
  const prefs = { speak: store.get("speakReplies", false), hands: false };

  const list = h("ul");
  const agentSel = agentSelect(app?.default || "");
  agentSel.title = "Agent for new chats";
  const convPane = h("div", { class: "convs" },
    h("div", { class: "top" },
      h("button", { type: "button", class: "primary", onclick: () => { newConv(); convPane.classList.remove("open"); box.focus(); } }, "New chat"),
      (app?.agents?.length || 0) > 1 ? agentSel : null),
    list);
  const title = h("div", { class: "grow" }, "Chat");
  const speakBox = h("input", { type: "checkbox", checked: prefs.speak });
  const handsBox = h("input", { type: "checkbox" });
  const log = h("div", { id: "log" });
  const typing = h("div", { id: "typing" });
  const chips = h("div", { class: "chips" });
  const meter = h("div", { class: "meter" });
  const box = h("textarea", { rows: "1", placeholder: "Message Habakkuk", autocomplete: "off", "aria-label": "Message" });
  const fileIn = h("input", { type: "file", multiple: true, hidden: true,
    accept: "image/*,application/pdf,.pdf,.doc,.docx,.xls,.xlsx,.ppt,.pptx,.txt,.md,.csv,.json,.html,.htm" });
  const attachBtn = h("button", { type: "button", class: "round", title: "Attach files", "aria-label": "Attach files" }, icon(ICONS.clip));
  const micBtn = h("button", { type: "button", class: "round", title: "Hold to talk, or tap to start and tap again to send", "aria-label": "Talk" }, icon(ICONS.mic));
  const sendBtn = h("button", { type: "submit", class: "primary send" }, "Send");
  const liveBtn = h("button", { type: "button", class: "btn small live", title: "Talk with Habakkuk in real time (interrupt any time)" }, "Live");
  const form = h("form", { class: "composer" }, attachBtn, fileIn, micBtn, box, sendBtn);
  const talk = h("div", { class: "talk" },
    h("div", { class: "talkbar" },
      h("button", { type: "button", class: "btn small show-sm", onclick: () => convPane.classList.toggle("open") }, "Chats"),
      title,
      app?.speech ? h("label", { class: "toggle", title: "Read replies aloud" }, speakBox, "Speak replies") : null,
      app?.transcription ? h("label", { class: "toggle", title: "Listen continuously and send when you stop talking" }, handsBox, "Hands-free") : null,
      app?.realtime ? liveBtn : null),
    meter, log, typing, chips, form);
  view.append(h("div", { class: "chat" }, convPane, talk));
  if (!app?.transcription) micBtn.hidden = true;

  const conv = () => convs.find(c => c.id === current);
  const save = () => { store.set("convs", convs); store.set("current", current); };
  function newConv() {
    const c = { id: "c-" + rid(), title: "New chat", agent: agentSel.value || "", updated: Date.now() };
    convs.unshift(c);
    open(c.id);
  }
  function renderList() {
    list.textContent = "";
    for (const c of convs) {
      list.append(h("li", { class: c.id === current ? "on" : "", onclick: () => { open(c.id); convPane.classList.remove("open"); } },
        h("span", {}, c.title || c.id), c.agent ? h("small", {}, c.agent) : null,
        h("button", { type: "button", class: "icon small", title: "Remove from this list", onclick: e => {
          e.stopPropagation();
          convs = convs.filter(x => x.id !== c.id);
          if (current === c.id) { if (convs[0]) open(convs[0].id); else newConv(); }
          save(); renderList();
        } }, "×")));
    }
  }
  async function merge() {
    try {
      const j = await api("chat/conversations");
      for (const sc of j.conversations) {
        let c = convs.find(x => x.id === sc.conversation);
        if (!c) { c = { id: sc.conversation, title: sc.title || sc.conversation, agent: sc.agent, updated: Date.parse(sc.last_at) }; convs.push(c); }
        else if ((c.title === "New chat" || !c.title) && sc.title) c.title = sc.title;
        if (!c.agent) c.agent = sc.agent;
      }
      convs.sort((a, b) => (b.updated || 0) - (a.updated || 0));
      save(); renderList();
    } catch {}
  }

  function open(id) {
    current = id;
    save(); renderList();
    const c = conv();
    title.textContent = c ? c.title : "Chat";
    $("title").textContent = "Chat";
    log.textContent = "";
    log.append(emptyState("Say hello, or try one of these:", STARTERS.map(st =>
      h("button", { type: "button", class: "btn small", title: st.text, onclick: () => { box.value = st.text; form.requestSubmit(); } }, st.label))));
    typing.textContent = "";
    lastSeq = 0; act = null; openedAt = Date.now();
    stopSpeaking();
    if (live.pc) stopLive();
    if (ctrl) ctrl.abort();
    ctrl = new AbortController();
    const sig = ctrl.signal;
    stream(() => `chat/${encodeURIComponent(id)}/events?after=${lastSeq}`, onMessage, sig, on => {
      $("dot").className = "dot" + (on ? " on" : "");
    });
    stream(() => `chat/${encodeURIComponent(id)}/live`, onLive, sig);
  }
  onLeave(() => { if (ctrl) ctrl.abort(); stopSpeaking(); stopHands(); stopLive(); micRelease(); });

  const nearBottom = () => log.scrollHeight - log.scrollTop - log.clientHeight < 120;
  function append(node, force) {
    const near = nearBottom();
    log.querySelector(".empty")?.remove();
    log.append(node);
    if (near || force) log.scrollTop = log.scrollHeight;
  }

  function onMessage(event, e) {
    if (event === "typing") {
      typing.textContent = "Working…";
      clearTimeout(typingTimer);
      typingTimer = setTimeout(() => { typing.textContent = ""; }, 7000);
      return;
    }
    if (event !== "message" || e.seq <= lastSeq) return;
    lastSeq = e.seq;
    if (e.role === "user") {
      append(h("div", { class: "msg user", title: when(e.at) }, e.text), true);
      return;
    }
    settle();
    typing.textContent = "";
    clearTimeout(typingTimer);
    const div = h("div", { class: "msg assistant", title: when(e.at) }, md(e.text));
    const acts = h("div", { class: "acts" });
    if (/^\*\*Approval needed:\*\*/.test(e.text)) {
      acts.append(
        h("button", { type: "button", class: "primary small", onclick: () => send("yes") }, "Approve"),
        h("button", { type: "button", class: "danger small", onclick: () => send("no") }, "Deny"));
    }
    if (app?.speech) acts.append(h("button", { type: "button", class: "ghost small", onclick: () => { unlockAudio(); speak(current, e.seq); } }, "Play"));
    if (acts.childNodes.length) div.append(acts);
    append(div);
    if (prefs.speak && !live.pc && Date.parse(e.at) > openedAt - 3000) speak(current, e.seq);
  }

  // Live activity: tool cards as calls run, the reply as it streams.
  function activity() {
    if (!act) {
      act = { group: h("div", { class: "activity" }), cards: new Map(), draft: null, text: "", tools: 0, gap: false };
      append(act.group);
    }
    return act;
  }
  function settle() {
    if (!act) return;
    act.draft?.remove();
    if (act.tools) {
      const det = h("details", { class: "activity" }, h("summary", {}, `${act.tools} tool call${act.tools === 1 ? "" : "s"}`));
      det.append(...act.group.childNodes);
      act.group.replaceWith(det);
    } else act.group.remove();
    act = null;
  }
  function what(input) {
    try {
      const j = JSON.parse(input);
      for (const k of ["command", "cmd", "path", "url", "query", "pattern", "name", "title", "slug", "text", "prompt"]) {
        if (typeof j[k] === "string") return j[k];
      }
      const first = Object.values(j).find(v => typeof v === "string");
      return first || "";
    } catch { return input.slice(0, 120); }
  }
  function pretty(t) { try { return JSON.stringify(JSON.parse(t), null, 2); } catch { return t; } }
  function onLive(event, v) {
    if (event !== "activity") return;
    const a = activity();
    switch (v.kind) {
      case "text":
        if (a.gap && a.text) a.text += "\n\n";
        a.gap = false;
        a.text += v.text;
        if (!a.draft) { a.draft = h("div", { class: "msg assistant draft" }); append(a.draft); }
        a.draft.textContent = "";
        a.draft.append(md(a.text));
        if (nearBottom()) log.scrollTop = log.scrollHeight;
        break;
      case "thinking":
        typing.textContent = "Thinking…";
        break;
      case "tool_start": {
        a.gap = true;
        a.tools++;
        const card = h("details", { class: "tool run" },
          h("summary", {}, h("span", { class: "state" }), h("span", { class: "name" }, v.tool), h("span", { class: "what" }, what(v.input || ""))),
          h("pre", {}, pretty(v.input || "")));
        a.cards.set(v.id, card);
        a.group.append(card);
        if (a.draft) a.group.after(a.draft);
        if (nearBottom()) log.scrollTop = log.scrollHeight;
        typing.textContent = "Running " + v.tool + "…";
        break;
      }
      case "tool_done": {
        const card = a.cards.get(v.id);
        if (!card) break;
        card.classList.remove("run");
        if (v.is_error) card.classList.add("fail");
        card.querySelector(".state").textContent = v.is_error ? "failed" : "done";
        card.querySelector(".state").className = "state badge " + (v.is_error ? "bad" : "ok");
        card.append(h("pre", {}, v.text || "(no output)"));
        break;
      }
      case "review":
        a.group.append(h("div", { class: "small muted" }, "Reviewer: " + v.text));
        break;
      case "retry":
        typing.textContent = "Retrying: " + (v.text || "");
        break;
      case "turn_error":
        a.group.append(h("div", { class: "small err" }, "Error: " + (v.text || "the turn failed")));
        break;
    }
  }

  // Sending
  function renderChips() {
    chips.textContent = "";
    files.forEach((f, i) => chips.append(h("span", { class: "chip", title: f.name },
      h("span", {}, f.name), h("button", { type: "button", "aria-label": "Remove " + f.name, onclick: () => { files.splice(i, 1); renderChips(); } }, "×"))));
  }
  attachBtn.addEventListener("click", () => fileIn.click());
  fileIn.addEventListener("change", () => {
    files.push(...fileIn.files);
    if (files.length > 10) { files = files.slice(0, 10); toast("At most 10 files per message", true); }
    fileIn.value = "";
    renderChips();
  });
  async function send(text, audio) {
    const c = conv();
    if (!c) return false;
    const id = "m-" + rid();
    try {
      if (files.length || audio) {
        const fd = new FormData();
        fd.append("conversation", c.id);
        fd.append("id", id);
        fd.append("text", text || "");
        if (c.agent) fd.append("agent", c.agent);
        for (const f of files) fd.append("file", f, f.name);
        if (audio) fd.append("audio", audio.blob, audio.name);
        await api("chat", { method: "POST", form: fd });
        files = [];
        renderChips();
      } else {
        const body = { conversation: c.id, text, id };
        if (c.agent) body.agent = c.agent;
        await api("chat", { method: "POST", body });
      }
    } catch (err) { fail(err); return false; }
    if (c.title === "New chat" && text) { c.title = text.slice(0, 48); title.textContent = c.title; }
    c.updated = Date.now();
    convs = [c, ...convs.filter(x => x !== c)];
    save(); renderList();
    return true;
  }
  form.addEventListener("submit", async e => {
    e.preventDefault();
    if (prefs.speak) unlockAudio();
    const text = box.value.trim();
    if (!text && !files.length) return;
    box.value = "";
    grow();
    sendBtn.disabled = true;
    if (!(await send(text))) box.value = text;
    sendBtn.disabled = false;
  });
  function grow() { box.style.height = "auto"; box.style.height = Math.min(box.scrollHeight, innerHeight * 0.4) + "px"; }
  box.addEventListener("input", grow);
  box.addEventListener("keydown", e => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !matchMedia("(pointer: coarse)").matches) {
      e.preventDefault();
      form.requestSubmit();
    }
  });
  speakBox.addEventListener("change", () => {
    prefs.speak = speakBox.checked;
    store.set("speakReplies", prefs.speak);
    if (prefs.speak) unlockAudio(); else stopSpeaking();
  });

  async function sendVoice(rec) {
    if (!rec || rec.blob.size < 800 || rec.ms < 300) { typing.textContent = ""; return; }
    typing.textContent = "Transcribing…";
    const ok = await send(box.value.trim(), rec);
    if (ok) { box.value = ""; grow(); }
    typing.textContent = "";
  }

  // Push to talk: hold the mic button, or tap to start and tap to send.
  let downAt = 0, tapMode = false;
  micBtn.addEventListener("pointerdown", async e => {
    e.preventDefault();
    if (prefs.hands) return;
    unlockAudio();
    if (recording()) { // second tap
      tapMode = false;
      micBtn.classList.remove("rec");
      sendVoice(await recStop());
      return;
    }
    stopSpeaking();
    downAt = Date.now();
    try { await recStart(); } catch (err) { fail(err); return; }
    micBtn.classList.add("rec");
    micBtn.setPointerCapture?.(e.pointerId);
  });
  const release = async () => {
    if (prefs.hands || !recording() || tapMode) return;
    if (Date.now() - downAt < 350) { tapMode = true; typing.textContent = "Listening… tap the mic to send"; return; }
    micBtn.classList.remove("rec");
    sendVoice(await recStop());
  };
  micBtn.addEventListener("pointerup", release);
  micBtn.addEventListener("pointercancel", release);
  micBtn.addEventListener("contextmenu", e => e.preventDefault());

  // Hands-free: a simple voice-activity detector. Speech starts when the
  // level rises well above the room's noise floor and ends after a stretch
  // of silence; it pauses while a reply is read aloud, so it does not hear
  // itself.
  let vadTimer = 0;
  const vad = { floor: 0.01, last: 0, voiced: 0, busy: false };
  async function startHands() {
    unlockAudio();
    try { await micStream(); } catch (err) { fail(err); handsBox.checked = false; prefs.hands = false; return; }
    micBtn.classList.add("listen");
    typing.textContent = "Listening…";
    vadTimer = setInterval(vadTick, 50);
  }
  function stopHands() {
    clearInterval(vadTimer);
    vadTimer = 0;
    micBtn.classList.remove("listen", "rec");
    meter.style.width = "0";
    if (recording()) recStop();
  }
  async function vadTick() {
    if (vad.busy || !mic.stream) return;
    if (speaker.playing) { meter.style.width = "0"; return; }
    const rms = level(), now = Date.now();
    meter.style.width = Math.min(100, rms * 600) + "%";
    const threshold = Math.max(0.02, vad.floor * 3.5);
    if (rms > threshold) {
      vad.last = now;
      vad.voiced += 50;
      if (!recording()) {
        vad.busy = true;
        try { await recStart(); micBtn.classList.add("rec"); } catch (err) { fail(err); }
        vad.busy = false;
      }
    } else if (!recording()) {
      vad.floor = vad.floor * 0.95 + rms * 0.05;
    } else if (now - vad.last > 1200 || now - mic.started > 60000) {
      vad.busy = true;
      micBtn.classList.remove("rec");
      const rec = await recStop();
      const voiced = vad.voiced;
      vad.voiced = 0;
      if (voiced >= 300) await sendVoice(rec);
      if (prefs.hands) typing.textContent = "Listening…";
      vad.busy = false;
    }
  }
  // Live mode: the browser talks to a realtime speech model over WebRTC
  // with a short-lived secret Habakkuk mints. The model handles the talk and
  // barge-in; its one tool, ask_habakkuk, is forwarded to Habakkuk, which runs a
  // normal turn with the agent's tools and returns the reply to be spoken.
  const live = { pc: null, dc: null, el: null, conv: "", agent: "", lines: [], timer: 0 };
  async function startLive() {
    const c = conv();
    if (!c || live.pc) return;
    if (prefs.hands) { handsBox.checked = false; prefs.hands = false; stopHands(); }
    stopSpeaking();
    liveBtn.disabled = true;
    typing.textContent = "Connecting…";
    try {
      const sess = await api("realtime/session", { method: "POST", body: { conversation: c.id, agent: c.agent || "" } });
      const pc = new RTCPeerConnection();
      live.pc = pc; live.conv = c.id; live.agent = c.agent || "";
      live.el = new Audio();
      live.el.autoplay = true;
      pc.ontrack = e => { live.el.srcObject = e.streams[0]; };
      pc.onconnectionstatechange = () => {
        if (["failed", "closed"].includes(pc.connectionState) && live.pc === pc) { toast("Live call ended", true); stopLive(); }
      };
      const st = await micStream();
      for (const t of st.getAudioTracks()) pc.addTrack(t, st);
      const dc = pc.createDataChannel("oai-events");
      live.dc = dc;
      dc.onmessage = e => { try { onRealtime(JSON.parse(e.data)); } catch {} };
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      const res = await fetch(sess.calls_url, {
        method: "POST", body: offer.sdp,
        headers: { Authorization: "Bearer " + sess.client_secret, "Content-Type": "application/sdp" },
      });
      if (!res.ok) throw new Error("the voice service refused the call (HTTP " + res.status + ")");
      await pc.setRemoteDescription({ type: "answer", sdp: await res.text() });
      live.timer = setInterval(flushLines, 1500);
      liveBtn.classList.add("on");
      liveBtn.textContent = "End live";
      typing.textContent = "Live: just talk";
    } catch (err) { fail(err); stopLive(); }
    liveBtn.disabled = false;
  }
  function stopLive() {
    if (!live.pc && !live.timer) return;
    clearInterval(live.timer);
    try { live.dc?.close(); } catch {}
    try { live.pc?.close(); } catch {}
    if (live.el) live.el.srcObject = null;
    const { conv: id, agent } = live;
    Object.assign(live, { pc: null, dc: null, el: null, timer: 0 });
    micRelease();
    liveBtn.classList.remove("on");
    liveBtn.textContent = "Live";
    typing.textContent = "";
    flushLines().finally(() => api("realtime/end", { method: "POST", body: { conversation: id, agent } }).catch(() => {}));
  }
  async function flushLines() {
    if (!live.lines.length) return;
    const lines = live.lines.splice(0, 50);
    try { await api("realtime/transcript", { method: "POST", body: { conversation: live.conv || current, lines } }); }
    catch (err) { live.lines.unshift(...lines); }
  }
  function rtSend(ev) { if (live.dc?.readyState === "open") live.dc.send(JSON.stringify(ev)); }
  function onRealtime(ev) {
    switch (ev.type) {
      case "conversation.item.input_audio_transcription.completed":
        if (ev.transcript?.trim()) live.lines.push({ role: "user", text: ev.transcript.trim() });
        break;
      case "response.output_audio_transcript.done":
      case "response.audio_transcript.done":
        if (ev.transcript?.trim()) live.lines.push({ role: "assistant", text: ev.transcript.trim() });
        break;
      case "input_audio_buffer.speech_started":
        typing.textContent = "Listening…";
        break;
      case "response.function_call_arguments.done":
        if (ev.name === "ask_habakkuk") askHabakkuk(ev.call_id, ev.arguments);
        break;
      case "error":
        toast("Live: " + (ev.error?.message || "error"), true);
        break;
    }
  }
  async function askHabakkuk(callId, args) {
    let request = "";
    try { request = JSON.parse(args || "{}").request || ""; } catch {}
    typing.textContent = "Habakkuk is working…";
    await flushLines();
    let output;
    try {
      const j = await api("realtime/tool", { method: "POST", body: { conversation: live.conv, agent: live.agent, call_id: callId, request } });
      output = j.output || "Done.";
    } catch (err) { output = "Habakkuk could not take the request: " + err.message; }
    rtSend({ type: "conversation.item.create", item: { type: "function_call_output", call_id: callId, output } });
    rtSend({ type: "response.create" });
    if (live.pc) typing.textContent = "Live: just talk";
  }
  liveBtn.addEventListener("click", () => { unlockAudio(); if (live.pc) stopLive(); else startLive(); });

  handsBox.addEventListener("change", () => {
    prefs.hands = handsBox.checked;
    if (prefs.hands) startHands(); else { stopHands(); typing.textContent = ""; }
  });

  renderList();
  if (draft) newConv(); // a suggestion from another page starts a fresh chat
  else if (conv()) open(current);
  else if (convs.length) open(convs[0].id);
  else newConv();
  merge();
  if (draft) { box.value = draft; draft = ""; grow(); box.focus(); }
};

// Sessions

pages.sessions = (view, [id, seq]) => {
  if (id) return transcript(view, id, seq ? Number(seq) : -1);
  const params = new URLSearchParams(location.hash.split("?")[1] || "");
  const q = h("input", { type: "search", placeholder: "Search messages", value: params.get("q") || "", "aria-label": "Search" });
  const agent = agentSelect("", { any: "All agents" });
  const out = h("div", { class: "panel" });
  const run = async () => {
    loading(out);
    try {
      const qs = new URLSearchParams();
      if (q.value.trim()) qs.set("q", q.value.trim());
      if (agent.value) qs.set("agent", agent.value);
      const j = await api("sessions?" + qs);
      out.textContent = "";
      if (j.hits) {
        out.append(j.hits.length ? h("ul", { class: "list" }, j.hits.map(x => h("li", { class: "click", onclick: () => go(`#/sessions/${x.session}/${x.seq}`) },
          h("div", { class: "row" }, h("strong", { class: "grow" }, x.title || x.session), badge(x.agent), h("span", { class: "muted small" }, ago(x.at))),
          h("div", { class: "snippet" }, x.role + ": " + x.snippet))))
          : h("div", { class: "empty" }, "No matches."));
      } else {
        out.append(j.sessions.length ? table(["Title", "Agent", "Updated"], j.sessions.map(x =>
          h("tr", { class: "click", onclick: () => go("#/sessions/" + x.id) },
            h("td", {}, x.title || h("span", { class: "muted mono small" }, x.id)), h("td", {}, x.agent),
            h("td", { title: when(x.updated_at) }, ago(x.updated_at)))))
          : emptyState("No sessions yet. Every conversation, from any channel, shows up here.", h("a", { class: "btn small", href: "#/chat" }, "Start a chat")));
      }
    } catch (err) { out.textContent = ""; out.append(h("div", { class: "empty err" }, err.message)); }
  };
  q.addEventListener("keydown", e => { if (e.key === "Enter") run(); });
  agent.addEventListener("change", run);
  view.append(h("div", { class: "toolbar" }, q, agent, h("button", { type: "button", class: "primary", onclick: run }, "Search")), out);
  run();
};

async function download(path, name) {
  try {
    const res = await api(path, { raw: true });
    const url = URL.createObjectURL(await res.blob());
    const a = h("a", { href: url, download: name });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 5000);
  } catch (err) { fail(err); }
}

async function transcript(view, id, seq) {
  loading(view);
  let j;
  try { j = await api("sessions/" + encodeURIComponent(id)); } catch (err) { view.textContent = ""; view.append(h("div", { class: "empty err" }, err.message)); return; }
  const sess = j.session;
  $("title").textContent = sess.title || "Session";
  view.textContent = "";
  view.append(h("div", { class: "toolbar" },
    h("a", { href: "#/sessions", class: "btn" }, "Back"),
    h("div", { class: "grow muted small" }, `${sess.agent} · ${sess.id} · started ${when(sess.created_at)}`),
    h("button", { type: "button", class: "btn", onclick: () => download(`sessions/${encodeURIComponent(id)}/export`, id + ".md") }, "Export")));
  if (j.compaction) {
    view.append(h("details", { class: "panel" }, h("summary", {}, `Summary of messages before #${j.compaction.keep_from}`), md(j.compaction.summary)));
  }
  let target = null;
  const box = h("div", { class: "panel" });
  for (const m of j.messages) {
    const div = h("div", { class: "turn " + m.role + (m.seq === seq ? " hit" : "") },
      h("div", { class: "who" }, `#${m.seq} ${m.role}${m.model ? " · " + m.model : ""}`));
    for (const b of m.blocks) {
      switch (b.type) {
        case "text": div.append(md(b.text)); break;
        case "tool_use":
          div.append(h("details", { class: "tool" }, h("summary", {}, h("span", { class: "name" }, b.tool), h("span", { class: "what" }, "call")),
            h("pre", {}, b.input)));
          break;
        case "tool_result":
          div.append(h("details", { class: "tool" + (b.is_error ? " fail" : "") },
            h("summary", {}, h("span", { class: "name" }, b.is_error ? "error" : "result"), h("span", { class: "what" }, (b.text || "").split("\n")[0])),
            h("pre", {}, b.text + (b.cut ? "\n…(cut)" : ""))));
          break;
        case "thinking":
          div.append(h("details", {}, h("summary", { class: "muted small" }, "thinking"), h("pre", {}, b.text)));
          break;
        case "image": case "document":
          div.append(h("div", { class: "muted small" }, `[${b.type}: ${b.name || b.media_type || ""}]`));
          break;
        default:
          div.append(h("div", { class: "muted small" }, `[${b.type}]`));
      }
    }
    if (m.seq === seq) target = div;
    box.append(div);
  }
  if (!j.messages.length) box.append(h("div", { class: "empty" }, "No messages."));
  view.append(box);
  target?.scrollIntoView({ block: "center" });
}

// Board

const STATUSES = ["todo", "doing", "blocked", "done", "cancelled"];
pages.board = view => {
  const filter = h("select", { "aria-label": "Show" }, h("option", { value: "open" }, "Open cards"), h("option", { value: "all" }, "All cards"));
  filter.value = store.get("boardFilter", "open");
  const cols = h("div", { class: "kanban" });
  const hint = h("div");
  const load = async () => {
    try {
      const j = await api("board?status=" + filter.value);
      cols.textContent = "";
      hint.textContent = "";
      if (!j.cards.length) {
        hint.append(h("div", { class: "panel" }, emptyState("The board holds tasks your agents work through on their own, in order, and report back. Add a card, or ask in Chat.",
          askBtn("Add a task by chat", "Put \"compare three phone plans for me\" on the board and work on it."))));
      }
      const shown = filter.value === "all" ? STATUSES : STATUSES.slice(0, 3);
      for (const st of shown) {
        const cards = j.cards.filter(c => c.status === st);
        cols.append(h("div", { class: "column" }, h("h3", {}, st, h("span", { class: "muted" }, cards.length)),
          cards.map(c => h("div", { class: "card", onclick: () => cardDialog(c.id, load) },
            h("div", { class: "t" }, c.title),
            h("div", { class: "m" }, h("span", {}, c.assignee || "unassigned"), c.priority ? h("span", {}, "p" + c.priority) : null,
              h("span", { title: when(c.updated_at) }, ago(c.updated_at)))))));
      }
    } catch (err) { cols.textContent = ""; cols.append(h("div", { class: "empty err" }, err.message)); }
  };
  filter.addEventListener("change", () => { store.set("boardFilter", filter.value); load(); });
  view.append(h("div", { class: "toolbar" }, filter, h("span", { class: "grow" }),
    h("button", { type: "button", class: "primary", onclick: () => newCard(load) }, "New card")), hint, cols);
  every(10000, load);
};

function newCard(done) {
  const t = h("input", { type: "text", required: true });
  const body = h("textarea", { rows: "5" });
  const who = agentSelect("", { any: "Unassigned" });
  const prio = h("input", { type: "number", value: "0", step: "1" });
  const f = h("form", {}, h("h2", {}, "New card"), field("Title", t), field("Details", body), field("Assignee", who), field("Priority (higher runs first)", prio),
    h("div", { class: "row end" }, h("button", { type: "button", class: "btn", onclick: closeModal }, "Cancel"), h("button", { type: "submit", class: "primary" }, "Create")));
  f.addEventListener("submit", async e => {
    e.preventDefault();
    try {
      await api("board", { method: "POST", body: { title: t.value, body: body.value, assignee: who.value, priority: Number(prio.value) || 0 } });
      closeModal();
      toast("Card created");
      done();
    } catch (err) { fail(err); }
  });
  modal(f);
  t.focus();
}

async function cardDialog(id, changed) {
  let c;
  try { c = await api("board/" + encodeURIComponent(id)); } catch (err) { fail(err); return; }
  const act = async (action, body) => {
    try { await api(`board/${encodeURIComponent(id)}/${action}`, { method: "POST", body }); changed(); cardDialog(id, changed); } catch (err) { fail(err); }
  };
  const open = c.status !== "done" && c.status !== "cancelled";
  const who = agentSelect(c.assignee, { any: "Unassigned" });
  const note = h("textarea", { rows: "3", placeholder: "Add a comment" });
  modal(
    h("div", { class: "row" }, h("h2", { class: "grow" }, c.title), h("button", { type: "button", class: "icon", "aria-label": "Close", onclick: closeModal }, "×")),
    h("div", { class: "row small" }, badge(c.status, stateKind(c.status)), h("span", { class: "muted" }, `for ${c.assignee || "nobody"} · by ${c.created_by} · priority ${c.priority} · attempt ${c.attempt}`),
      c.session ? h("a", { href: "#/sessions/" + c.session }, "session") : null),
    c.body ? h("div", { class: "panel" }, md(c.body)) : null,
    c.result ? h("div", { class: "panel" }, h("h3", {}, "Result"), md(c.result)) : null,
    h("h3", {}, "Comments"),
    c.comments?.length ? h("ul", { class: "list" }, c.comments.map(x => h("li", {}, h("div", { class: "small muted" }, `${x.author} · ${when(x.at)}`), md(x.text))))
      : h("div", { class: "muted small" }, "None yet."),
    h("div", { class: "row" }, note, h("button", { type: "button", class: "btn", onclick: () => note.value.trim() && act("comment", { text: note.value }) }, "Comment")),
    open ? h("div", { class: "row", style: null }, who, h("button", { type: "button", class: "btn", onclick: () => act("assign", { assignee: who.value }) }, "Assign"),
      h("span", { class: "grow" }),
      c.status === "blocked" ? h("button", { type: "button", class: "btn", onclick: () => act("reopen") }, "Reopen") : null,
      h("button", { type: "button", class: "danger", onclick: () => confirm("Cancel this card?") && act("cancel") }, "Cancel card")) : null,
    c.events?.length ? h("details", {}, h("summary", { class: "muted small" }, "History"),
      h("ul", { class: "list small" }, c.events.map(x => h("li", {}, `${when(x.at)} · ${x.author} ${x.kind} ${x.text}`)))) : null,
  );
  note.classList.add("grow");
}

// Schedules

pages.schedules = view => {
  const out = h("div", { class: "panel" });
  const load = async () => {
    try {
      const j = await api("schedules");
      out.textContent = "";
      out.append(j.schedules.length ? table(["Name", "When", "Next", h("th", { class: "hide-sm" }, "Last"), "", ""], j.schedules.map(sc => {
        const act = async (a, method) => {
          try {
            if (method === "DELETE") await api("schedules/" + sc.id, { method });
            else await api(`schedules/${sc.id}/${a}`, { method: "POST" });
            toast(a === "run" ? "Queued a run" : "Done");
            load();
          } catch (err) { fail(err); }
        };
        return h("tr", {},
          h("td", {}, h("strong", {}, sc.name), h("div", { class: "small" }, plainWhat(sc)),
            h("details", { class: "more" }, h("summary", {}, "Details"), h("div", { class: "small muted" }, "Agent: " + sc.agent),
              sc.command ? h("div", { class: "small snippet mono" }, sc.command) : null, sc.prompt ? h("div", { class: "small snippet" }, sc.prompt) : null)),
          h("td", { class: "small", title: sc.spec }, plainWhen(sc.spec)),
          h("td", { class: "small", title: when(sc.next_run) }, sc.enabled ? ago(sc.next_run) : badge("paused")),
          h("td", { class: "small hide-sm", title: sc.last_output || "" }, sc.last_run ? ago(sc.last_run) : "", sc.last_status ? h("div", { class: "muted" }, sc.last_status) : null),
          h("td", {}, h("div", { class: "row" },
            h("button", { type: "button", class: "btn small", onclick: () => act(sc.enabled ? "pause" : "resume") }, sc.enabled ? "Pause" : "Resume"),
            h("button", { type: "button", class: "btn small", onclick: () => act("run") }, "Run now"))),
          h("td", {}, h("button", { type: "button", class: "danger small", onclick: () => confirm(`Delete "${sc.name}"?`) && act("delete", "DELETE") }, "Delete")));
      })) : emptyState("No schedules yet. Ask your agent in plain words and it sets one up, or use Add a prompt schedule above.",
        askBtn(STARTERS[0].label, STARTERS[0].text), askBtn("Weekly review", "Every Sunday at 6pm, ask me what went well this week and save my answer.")));
    } catch (err) { out.textContent = ""; out.append(h("div", { class: "empty err" }, err.message)); }
  };
  const agent = agentSelect(app?.default || "");
  const name = h("input", { type: "text", placeholder: "morning-brief" });
  const spec = h("input", { type: "text", placeholder: "0 8 * * 1-5, @every 2h or at 2026-12-01 09:00" });
  const prompt = h("textarea", { rows: "3", placeholder: "What the agent is asked each run" });
  const deliver = h("input", { type: "text", placeholder: "optional: ACCOUNT:CONVERSATION, e.g. web:c-1234" });
  const chat = store.get("current", "");
  const add = h("form", {}, h("div", { class: "row" }, field("Agent", agent), h("div", { class: "grow" }, field("Name", name))),
    field("When", spec), field("Prompt", prompt), field("Deliver results to", deliver),
    h("div", { class: "row end" },
      chat && app?.account ? h("button", { type: "button", class: "btn", onclick: () => { deliver.value = app.account + ":" + chat; } }, "Use my current chat") : null,
      h("button", { type: "submit", class: "primary" }, "Add schedule")));
  add.addEventListener("submit", async e => {
    e.preventDefault();
    try {
      await api("schedules", { method: "POST", body: { agent: agent.value, name: name.value, when: spec.value, prompt: prompt.value, deliver_to: deliver.value.trim() } });
      toast("Schedule added");
      add.reset();
      load();
    } catch (err) { fail(err); }
  });
  view.append(h("details", { class: "panel" }, h("summary", {}, "Add a prompt schedule"), h("div", { class: "small muted" },
    "Times are in the server's timezone" + (app?.timezone ? ` (${app.timezone})` : "") + ". Script schedules are added with hk schedules add -run."), add), out);
  every(15000, load);
};

// Memory

pages.memory = (view, [agentName]) => {
  const agent = agentSelect(agentName || store.get("memoryAgent", app?.default || ""));
  const q = h("input", { type: "search", placeholder: "Search notes", "aria-label": "Search notes" });
  const out = h("div", { class: "panel" });
  const load = async () => {
    store.set("memoryAgent", agent.value);
    loading(out);
    try {
      const qs = q.value.trim() ? "?q=" + encodeURIComponent(q.value.trim()) : "";
      const j = await api(`memory/${encodeURIComponent(agent.value)}${qs}`);
      out.textContent = "";
      out.append(h("div", { class: "muted small" }, qs ? `${j.memories.length} match(es)` : `${j.total} note(s)`));
      out.append(j.memories.length ? h("ul", { class: "list" }, j.memories.map(m => h("li", { class: "click", onclick: () => editNote(agent.value, m.slug, load) },
        h("div", { class: "row" }, h("strong", { class: "grow" }, m.title), h("span", { class: "muted small" }, ago(m.updated_at))),
        m.tags.length ? h("div", { class: "row" }, m.tags.map(t => badge(t, "info"))) : null,
        m.snippet ? h("div", { class: "snippet" }, m.snippet) : null)))
        : qs ? emptyState("No notes match.") : emptyState("No notes yet. Tell your agent something worth keeping and it saves a note here; memory review adds more when a chat goes quiet.",
          askBtn(STARTERS[1].label, STARTERS[1].text)));
    } catch (err) { out.textContent = ""; out.append(h("div", { class: "empty err" }, err.message)); }
  };
  agent.addEventListener("change", load);
  q.addEventListener("keydown", e => { if (e.key === "Enter") load(); });
  view.append(h("div", { class: "toolbar" }, agent, q, h("button", { type: "button", class: "btn", onclick: load }, "Search"),
    h("button", { type: "button", class: "primary", onclick: () => editNote(agent.value, "", load) }, "New note")), out);
  load();
};

async function editNote(agentName, slug, changed) {
  let m = { title: "", tags: [], body: "" };
  if (slug) {
    try { m = await api(`memory/${encodeURIComponent(agentName)}/${encodeURIComponent(slug)}`); } catch (err) { fail(err); return; }
  }
  const t = h("input", { type: "text", value: m.title, required: true });
  const tags = h("input", { type: "text", value: (m.tags || []).join(", "), placeholder: "comma, separated" });
  const body = h("textarea", { rows: "12", value: m.body, required: true });
  const f = h("form", {},
    h("div", { class: "row" }, h("h2", { class: "grow" }, slug ? "Edit note" : "New note"), h("button", { type: "button", class: "icon", "aria-label": "Close", onclick: closeModal }, "×")),
    slug ? h("div", { class: "muted small" }, `${agentName} · ${slug}.md · updated ${when(m.updated_at)}`) : null,
    field("Title", t), field("Tags", tags), field("Content", body),
    h("div", { class: "row" },
      slug ? h("button", { type: "button", class: "danger", onclick: async () => {
        if (!confirm("Forget this note? It moves to the agent's memory/.trash folder.")) return;
        try { await api(`memory/${encodeURIComponent(agentName)}/${encodeURIComponent(slug)}`, { method: "DELETE" }); closeModal(); toast("Forgotten"); changed(); } catch (err) { fail(err); }
      } }, "Forget") : null,
      h("span", { class: "grow" }),
      h("button", { type: "button", class: "btn", onclick: closeModal }, "Cancel"),
      h("button", { type: "submit", class: "primary" }, "Save")));
  f.addEventListener("submit", async e => {
    e.preventDefault();
    const payload = { title: t.value, content: body.value, tags: tags.value.split(",").map(x => x.trim()).filter(Boolean) };
    try {
      const path = `memory/${encodeURIComponent(agentName)}` + (slug ? "/" + encodeURIComponent(slug) : "");
      await api(path, { method: slug ? "PUT" : "POST", body: payload });
      closeModal();
      toast("Saved");
      changed();
    } catch (err) { fail(err); }
  });
  modal(f);
}

// Skills

pages.skills = (view, [agentName]) => {
  const agent = agentSelect(agentName || store.get("skillsAgent", app?.default || ""));
  const out = h("div", { class: "panel" });
  const load = async () => {
    store.set("skillsAgent", agent.value);
    loading(out);
    try {
      const j = await api("skills/" + encodeURIComponent(agent.value));
      out.textContent = "";
      const flip = async (e, sk) => {
        e.stopPropagation();
        try { await api("skills-off/" + encodeURIComponent(sk.name), { method: "POST", body: { off: !sk.off } }); toast(sk.off ? "Turned on" : "Turned off"); load(); }
        catch (err) { fail(err); }
      };
      out.append(h("div", { class: "muted small" }, "Turning a skill off turns it off for every agent. Nothing is deleted."));
      out.append(j.skills.length ? h("ul", { class: "list" }, j.skills.map(sk => h("li", { class: "click", onclick: () => viewSkill(agent.value, sk.name) },
        h("div", { class: "row" }, h("strong", { class: "grow mono" }, sk.name), sk.off ? badge("off", "bad") : null, badge(sk.shared ? "shared" : "own", sk.shared ? "" : "info"),
          h("button", { type: "button", class: "btn small", disabled: sk.off_in_config, title: sk.off_in_config ? "Turned off in config.yaml (skills.disabled)" : "",
            onclick: e => flip(e, sk) }, sk.off ? "Turn on" : "Turn off")),
        h("div", { class: "snippet" }, sk.description), sk.note ? h("div", { class: "small muted" }, sk.note) : null)))
        : emptyState("No skills yet. A skill is a routine your agent follows the same way every time. Ask it to write one, install one with hk skills install, or add a folder with a SKILL.md under skills/.",
          askBtn("Write a skill", "Write a skill for how you should summarise long articles for me: three bullets, then one line on why it matters.")));
    } catch (err) { out.textContent = ""; out.append(h("div", { class: "empty err" }, err.message)); }
  };
  agent.addEventListener("change", load);
  view.append(h("div", { class: "toolbar" }, agent), out);
  load();
};

async function viewSkill(agentName, name) {
  let sk;
  try { sk = await api(`skills/${encodeURIComponent(agentName)}/${encodeURIComponent(name)}`); } catch (err) { fail(err); return; }
  modal(
    h("div", { class: "row" }, h("h2", { class: "grow mono" }, sk.name), badge(sk.shared ? "shared" : "own"),
      h("button", { type: "button", class: "icon", "aria-label": "Close", onclick: closeModal }, "×")),
    h("p", { class: "muted" }, sk.description),
    h("div", { class: "panel" }, md(sk.body)),
    sk.files?.length ? h("div", {}, h("h3", {}, "Files"), h("ul", { class: "list mono small" }, sk.files.map(f => h("li", {}, f)))) : null);
}

// Usage

pages.usage = view => {
  const days = h("select", { "aria-label": "Period" }, [7, 30, 90, 365].map(d => h("option", { value: d }, `Last ${d} days`)));
  days.value = String(store.get("usageDays", 30));
  const agent = agentSelect("", { any: "All agents" });
  const metric = h("select", { "aria-label": "Chart" }, h("option", { value: "cost" }, "Cost"), h("option", { value: "tokens" }, "Tokens"));
  const out = h("div");
  const load = async () => {
    store.set("usageDays", Number(days.value));
    loading(out);
    try {
      const qs = new URLSearchParams({ days: days.value });
      if (agent.value) qs.set("agent", agent.value);
      const j = await api("usage?" + qs);
      const t = j.total;
      const stat = (n, l) => h("div", { class: "panel stat" }, h("div", { class: "n" }, n), h("div", { class: "l" }, l));
      const rows = list => table(["Name", h("th", { class: "num" }, "Calls"), h("th", { class: "num" }, "Input"), h("th", { class: "num" }, "Output"),
        h("th", { class: "num hide-sm" }, "Cache read"), h("th", { class: "num" }, "Cost")],
        list.map(r => h("tr", {}, h("td", { class: "mono small" }, r.key), h("td", { class: "num" }, r.calls), h("td", { class: "num" }, tokens(r.input_tokens)),
          h("td", { class: "num" }, tokens(r.output_tokens)), h("td", { class: "num hide-sm" }, tokens(r.cache_read_tokens)),
          h("td", { class: "num", title: r.unpriced ? `${r.unpriced} call(s) with no known price` : "" }, money(r.cost) + (r.unpriced ? "*" : "")))));
      out.textContent = "";
      out.append(h("div", { class: "grid" }, stat(money(t.cost), "cost"), stat(t.calls, "model calls"), stat(tokens(t.input_tokens), "input tokens"),
        stat(tokens(t.output_tokens), "output tokens"), stat(tokens(t.cache_read_tokens), "cache reads")));
      out.append(h("div", { class: "panel" }, h("h2", {}, metric.value === "cost" ? "Cost by day" : "Tokens by day"), chart(j.days, metric.value),
        h("div", { class: "muted small" }, `Days in ${j.timezone}.`)));
      out.append(h("div", { class: "panel" }, h("h2", {}, "By model"), j.by_model.length ? rows(j.by_model) : h("div", { class: "empty" }, "No model calls yet. Tokens and cost show up here after your first chat.")));
      out.append(h("div", { class: "panel" }, h("h2", {}, "By agent"), j.by_agent.length ? rows(j.by_agent) : h("div", { class: "empty" }, "No calls.")));
      if (t.unpriced) out.append(h("div", { class: "muted small" }, "* some calls used a model with no known price; set pricing in config.yaml."));
    } catch (err) { out.textContent = ""; out.append(h("div", { class: "empty err" }, err.message)); }
  };
  for (const c of [days, agent, metric]) c.addEventListener("change", load);
  view.append(h("div", { class: "toolbar" }, days, agent, metric), out);
  load();
};

function chart(days, metric) {
  const val = d => metric === "cost" ? d.cost : d.input_tokens + d.output_tokens + d.cache_read_tokens + d.cache_write_tokens;
  const label = v => metric === "cost" ? money(v) : tokens(v);
  const W = 720, H = 220, L = 52, B = 22, T = 10;
  const maxV = Math.max(...days.map(val), metric === "cost" ? 0.01 : 10);
  const bw = (W - L) / Math.max(days.length, 1);
  const y = v => T + (H - T - B) * (1 - v / maxV);
  const svg = s("svg", { class: "chart", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": "Usage by day" });
  for (const f of [0, 0.5, 1]) {
    svg.append(s("line", { class: "axis", x1: L, x2: W, y1: y(maxV * f), y2: y(maxV * f) }));
    svg.append(s("text", { x: L - 6, y: y(maxV * f) + 4, "text-anchor": "end" }, label(maxV * f)));
  }
  const step = Math.ceil(days.length / 8);
  days.forEach((d, i) => {
    const v = val(d), x = L + i * bw;
    const bar = s("rect", { class: "bar", x: x + bw * 0.15, width: Math.max(bw * 0.7, 1), y: y(v), height: Math.max(H - B - y(v), v > 0 ? 1 : 0), rx: 2 });
    bar.append(s("title", {}, `${d.key}: ${label(v)} (${d.calls} calls)`));
    svg.append(bar);
    if (i % step === 0) svg.append(s("text", { x: x + bw / 2, y: H - 6, "text-anchor": "middle" }, d.key.slice(5)));
  });
  return svg;
}

// Approvals

pages.approvals = view => {
  const out = h("div");
  every(3000, async () => {
    let j;
    try { j = await api("approvals"); } catch (err) { out.textContent = ""; out.append(h("div", { class: "empty err" }, err.message)); return; }
    setApprovalCount(j.approvals.length);
    out.textContent = "";
    if (!j.approvals.length) { out.append(h("div", { class: "panel empty" }, "Nothing is waiting for approval.")); return; }
    for (const a of j.approvals) {
      const decide = async ok => {
        try { await api("approvals/" + a.id, { method: "POST", body: { approve: ok } }); toast(ok ? "Approved" : "Denied"); route(); } catch (err) { fail(err); }
      };
      out.append(h("div", { class: "panel" },
        h("div", { class: "row" }, h("code", {}, a.tool), h("span", { class: "grow muted small" }, `${a.to.account}:${a.to.conversation} · waiting ${ago(a.since).replace(" ago", "")}`)),
        h("pre", {}, a.summary),
        a.review ? h("div", { class: "small" }, h("strong", {}, "Reviewer unsure: "), a.review) : null,
        h("div", { class: "row" }, h("a", { href: "#/sessions/" + a.session, class: "small" }, "Open session"), h("span", { class: "grow" }),
          h("button", { type: "button", class: "danger", onclick: () => decide(false) }, "Deny"),
          h("button", { type: "button", class: "primary", onclick: () => decide(true) }, "Approve"))));
    }
  });
  view.append(out);
  // Saved rules: what each agent may do without asking. Never in a turn that
  // read outside content, never for a guest.
  const rules = h("div", { class: "panel" });
  const loadRules = async () => {
    let j;
    try { j = await api("approval-rules"); } catch (err) { rules.textContent = ""; rules.append(h("div", { class: "empty err" }, err.message)); return; }
    rules.textContent = "";
    rules.append(h("h3", {}, "Always allowed"),
      h("p", { class: "small muted" }, "Calls an agent makes without asking, saved by Always allow or set in agents.<name>.allow. An exec rule is one simple command in one folder. Rules answer only for a message you typed just now as plain text, one to one or here, and a rule saved in a chat applies in that chat only. They never apply to schedules, voice, board cards, sub-agents, the API or another agent's task, nor after anything but your own words came in."));
    if (!j.rules.length) { rules.append(h("div", { class: "empty" }, "No rules yet.")); return; }
    rules.append(table(["Agent", "Covers", "From", ""], j.rules.map(r => h("tr", {},
      h("td", {}, r.agent),
      h("td", { class: "mono small" }, r.text),
      h("td", { class: "small muted" }, r.source + (r.added ? " · " + r.added.slice(0, 10) : ""),
        h("div", {}, "applies " + r.where + (r.saved_by ? ", saved by " + r.saved_by : ""))),
      h("td", {}, r.source === "config" ? h("span", { class: "small muted" }, "edit config.yaml") :
        h("button", { type: "button", class: "danger small", onclick: async () => {
          if (!confirm("Remove this rule? The agent asks again next time.")) return;
          try { await api(`approval-rules/${encodeURIComponent(r.agent)}/${encodeURIComponent(r.id)}`, { method: "DELETE" }); toast("Removed"); loadRules(); } catch (err) { fail(err); }
        } }, "Remove"))))));
  };
  loadRules();
  view.append(rules);
};
function setApprovalCount(n) {
  const c = $("approvalCount");
  c.textContent = n;
  c.hidden = !n;
}

// Config

pages.config = async view => {
  loading(view);
  let j;
  try { j = await api("config"); } catch (err) { view.textContent = ""; view.append(h("div", { class: "empty err" }, err.message)); return; }
  view.textContent = "";
  if (j.doctor) {
    view.append(h("div", { class: "panel" }, h("h2", {}, "Doctor"),
      h("ul", { class: "list" }, j.doctor.map(c => h("li", { class: "row" }, badge(c.ok ? "ok" : "problem", c.ok ? "ok" : "bad"),
        h("strong", {}, c.name), h("span", { class: "muted grow" }, c.detail))))));
  }
  view.append(await channelSettingsPanel());
  view.append(pushPanel());
  const cfg = j.config || {};
  view.append(h("div", { class: "panel" }, h("h2", {}, "Effective config"),
    h("div", { class: "muted small" }, `Secrets are redacted. Home: ${j.home || ""}`),
    Object.keys(cfg).sort().map(k => h("details", {}, h("summary", { class: "mono" }, k), h("pre", {}, JSON.stringify(cfg[k], null, 2))))));
};

// What each chat shows: two settings per channel, saved to config.yaml and
// used from the next message with no restart.
const PROGRESS_LABELS = { plain: "a short status line", detailed: "every step", off: "nothing" };
const DETAIL_LABELS = { summary: "a plain summary", full: "the exact command" };
async function channelSettingsPanel() {
  const panel = h("div", { class: "panel" }, h("h2", {}, "What your chats show"));
  let j;
  try { j = await api("channels"); } catch (err) { panel.append(h("div", { class: "empty err" }, err.message)); return panel; }
  if (!j.channels.length) {
    panel.append(emptyState("No chat channels yet. Run hk setup channel in a terminal to add WhatsApp, Telegram, Slack and more."));
    return panel;
  }
  panel.append(h("p", { class: "muted small" },
    "Changes are saved to config.yaml (the previous file is kept as config.yaml.bak) and apply from the next message. No restart is needed."));
  for (const c of j.channels) {
    const pick = (label, key, values, labels, current) => {
      const sel = h("select", { "aria-label": `${label} (${c.account})`, disabled: !j.writable },
        values.map(v => h("option", { value: v, selected: v === current }, labels[v] || v)));
      sel.addEventListener("change", async () => {
        sel.disabled = true;
        try {
          const saved = await api("channels/" + encodeURIComponent(c.account), { method: "PUT", body: { [key]: sel.value } });
          sel.value = saved[key];
          toast(`Saved for ${c.account}. It applies from the next message.`);
        } catch (err) {
          sel.value = current;
          toast(err.message, true);
          return;
        } finally { sel.disabled = !j.writable; }
        current = sel.value;
      });
      return h("label", { class: "field" }, label, sel);
    };
    panel.append(h("div", { class: "channel-settings" },
      h("div", { class: "row" }, h("strong", {}, c.account), h("span", { class: "muted" }, c.type)),
      pick("While working, show", "progress", j.progress, PROGRESS_LABELS, c.progress),
      !c.live_progress && h("div", { class: "muted small" },
        "This platform cannot edit a message, so a long task shows a typing indicator or one short notice instead of a line that changes."),
      pick("Approval requests show", "approvals_detail", j.approvals_detail, DETAIL_LABELS, c.approvals_detail)));
  }
  if (!j.writable) panel.append(h("div", { class: "muted small" }, "These settings cannot be saved from this process."));
  return panel;
}

// Notifications: Web Push to this device, straight from this server
// through the browser's own push service.
const PUSH_LABELS = {
  approvals: "An approval is waiting",
  replies: "A reply came while no dashboard showed that chat",
  schedules: "A scheduled task failed",
};
function b64Bytes(s) {
  const b = atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4));
  return Uint8Array.from(b, c => c.charCodeAt(0));
}
function deviceLabel() {
  const ua = navigator.userAgent;
  const browser = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "Browser";
  const os = /iPhone/.test(ua) ? "iPhone" : /iPad/.test(ua) ? "iPad" : /Android/.test(ua) ? "Android" : /Mac/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
  return os ? `${browser} on ${os}` : browser;
}
async function swReady() {
  if (!("serviceWorker" in navigator)) return null;
  if (!(await navigator.serviceWorker.getRegistration("./").catch(() => null))) return null; // registration failed
  return Promise.race([navigator.serviceWorker.ready, new Promise(res => setTimeout(() => res(null), 5000))]);
}
// pushPanel returns at once and fills itself in, so the page never waits
// on the service worker.
function pushPanel() {
  const panel = h("div", { class: "panel" }, h("h2", {}, "Notifications"));
  const body = h("div", { class: "muted small" }, "Loading…");
  let j;
  panel.append(h("p", { class: "muted small" },
    "Get a notification on this device when an approval is waiting, a reply comes while the dashboard is closed, or a scheduled task fails. " +
    "They come from this server through your browser's own push service; nothing else is involved."), body);
  const supported = window.isSecureContext && "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
  const render = async () => {
    if (!j) {
      try { j = await api("push"); } catch (err) { body.textContent = ""; body.append(h("div", { class: "empty err" }, err.message)); return; }
    }
    body.className = "";
    body.textContent = "";
    let sub = null;
    const reg = supported ? await swReady() : null;
    if (reg) sub = await reg.pushManager.getSubscription().catch(() => null);
    let mine = j.devices.find(d => d.id === store.get("pushId", ""));
    if (sub && !mine) {
      // The browser is subscribed but the server forgot it: register again.
      try {
        const out = await api("push", { method: "POST", body: { subscription: sub.toJSON(), label: deviceLabel() } });
        store.set("pushId", out.id);
        j = await api("push");
        mine = j.devices.find(d => d.id === out.id);
      } catch {}
    }
    if (!supported) {
      body.append(h("p", { class: "small" }, window.isSecureContext
        ? "This browser can't receive notifications from the dashboard. On an iPhone or iPad, add the dashboard to your home screen first, then open it from there."
        : "Notifications need the dashboard on https or on localhost."));
    } else if (!reg) {
      body.append(h("p", { class: "small" }, "The dashboard's service worker isn't running, so notifications can't be turned on. Reload the page and try again."));
    } else if (!sub || !mine) {
      body.append(h("div", { class: "row" }, h("span", { class: "grow" }, "Off on this device."),
        h("button", { type: "button", class: "primary", onclick: async e => {
          e.target.disabled = true;
          try {
            if (await Notification.requestPermission() !== "granted") throw new Error("Notifications are blocked for this site. Allow them in the browser's site settings.");
            const old = await reg.pushManager.getSubscription();
            if (old) await old.unsubscribe();
            const s = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64Bytes(j.public_key) });
            const out = await api("push", { method: "POST", body: { subscription: s.toJSON(), label: deviceLabel() } });
            store.set("pushId", out.id);
            j = await api("push");
            toast("Notifications are on for this device.");
          } catch (err) { toast(err.message, true); }
          render();
        } }, "Turn on")));
    } else {
      body.append(h("div", { class: "row" }, h("span", { class: "grow" }, "On for this device (", mine.label || "this browser", ")."),
        h("button", { type: "button", onclick: async () => {
          try { await api(`push/${mine.id}/test`, { method: "POST" }); toast("Sent. It should show in a few seconds."); } catch (err) { toast(err.message, true); }
        } }, "Send a test"),
        h("button", { type: "button", class: "danger", onclick: async () => {
          try { await sub.unsubscribe(); await api("push/" + mine.id, { method: "DELETE" }); store.set("pushId", ""); j = await api("push"); }
          catch (err) { toast(err.message, true); }
          render();
        } }, "Turn off")));
      for (const k of j.kinds) {
        const box = h("input", { type: "checkbox", checked: mine.kinds[k] });
        box.addEventListener("change", async () => {
          try { mine = await api("push/" + mine.id, { method: "PUT", body: { [k]: box.checked } }); }
          catch (err) { box.checked = !box.checked; toast(err.message, true); }
        });
        body.append(h("label", { class: "row small" }, box, PUSH_LABELS[k] || k));
      }
    }
    const others = j.devices.filter(d => d.id !== mine?.id);
    if (others.length) {
      body.append(h("h3", {}, "Other devices"), h("ul", { class: "list" }, others.map(d => h("li", { class: "row" },
        h("span", { class: "grow" }, d.label || "A browser", h("span", { class: "muted small" }, ` · ${d.service} · added ${ago(d.created)}`)),
        h("button", { type: "button", class: "small danger", onclick: async () => {
          try { await api("push/" + d.id, { method: "DELETE" }); j = await api("push"); render(); } catch (err) { toast(err.message, true); }
        } }, "Remove")))));
    }
  };
  render();
  return panel;
}

// Control panel

// Plain words for schedules: "@every 1m" reads "Every minute", "0 10 * * 2,4"
// reads "Tuesdays and Thursdays at 10am". Anything unusual shows as written.

const DAY_NAMES = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
function clock(hh, mm) {
  const h24 = +hh, m = +mm, ampm = h24 < 12 ? "am" : "pm", h12 = h24 % 12 || 12;
  return m ? `${h12}:${String(m).padStart(2, "0")}${ampm}` : `${h12}${ampm}`;
}
function plainDays(dow) {
  if (dow === "*") return "Every day";
  if (dow === "1-5") return "Weekdays";
  if (dow === "0,6" || dow === "6,0" || dow === "6-7") return "Weekends";
  let m = dow.match(/^(\d)-(\d)$/);
  if (m) return `${DAY_NAMES[+m[1] % 7]} to ${DAY_NAMES[+m[2] % 7]}`;
  if (/^\d(,\d)*$/.test(dow)) {
    const names = dow.split(",").map(d => DAY_NAMES[+d % 7] + "s");
    return names.length === 1 ? names[0] : names.slice(0, -1).join(", ") + " and " + names.at(-1);
  }
  return null;
}
function plainWhen(spec) {
  spec = String(spec || "").trim();
  let m;
  if ((m = spec.match(/^@every\s+(\d+)\s*([smhd])$/))) {
    const n = +m[1], unit = { s: "second", m: "minute", h: "hour", d: "day" }[m[2]];
    return n === 1 ? `Every ${unit}` : `Every ${n} ${unit}s`;
  }
  const named = { "@hourly": "Every hour", "@daily": "Every day at midnight", "@midnight": "Every day at midnight", "@weekly": "Every Sunday at midnight", "@monthly": "On the 1st of every month" };
  if (named[spec]) return named[spec];
  if ((m = spec.match(/^at\s+(.+)$/i))) return "Once, " + (when(m[1]) || m[1]);
  const f = spec.split(/\s+/);
  if (f.length !== 5) return spec;
  const [mi, ho, dom, mon, dow] = f;
  if (dom !== "*" || mon !== "*") return spec;
  if (mi === "*" && ho === "*" && dow === "*") return "Every minute";
  if ((m = mi.match(/^\*\/(\d+)$/)) && ho === "*") {
    const d = plainDays(dow);
    return d && `Every ${m[1]} minutes` + (dow === "*" ? "" : ", " + d.toLowerCase());
  }
  if (/^\d+$/.test(mi) && ho === "*" && dow === "*") return `Every hour at ${String(mi).padStart(2, "0")} past`;
  if (/^\d+$/.test(mi) && /^\d+(,\d+)*$/.test(ho)) {
    const d = plainDays(dow);
    if (!d) return spec;
    const times = ho.split(",").map(x => clock(x, mi));
    return `${d} at ${times.length === 1 ? times[0] : times.slice(0, -1).join(", ") + " and " + times.at(-1)}`;
  }
  return spec;
}
// What a schedule does, in a line: its prompt's first sentence, or that it
// runs a script.
function plainWhat(sc) {
  if (sc.command) return sc.on_output === "agent" ? "Runs a script, then the agent acts on what it finds" : "Runs a script";
  const p = String(sc.prompt || "").replace(/^\[[^\]]*\]\s*/, "").trim();
  const first = p.split(/(?<=[.!?])\s|\n/)[0] || p;
  return first.length > 140 ? first.slice(0, 137) + "…" : first;
}

// Sign-in link: an address ending in #token=... signs this browser in. The
// part after # is never sent to the server, and it is removed at once.
(function signInLink() {
  const m = location.hash.match(/^#token=([^&]+)/);
  if (!m) return;
  token = decodeURIComponent(m[1]);
  store.set("token", token);
  history.replaceState(null, "", location.pathname + location.search + "#/overview");
})();

// Saved changes that wait for a restart, and the button that applies them.

function showRestart(r) {
  if (app) app.restart = r;
  const bar = $("restartBar");
  bar.textContent = "";
  if (!r || !r.pending?.length) { bar.hidden = true; return; }
  bar.append(h("span", { class: "grow" }, "You saved changes that take effect after a restart (" + r.pending.join(", ") + ")."),
    r.can_restart ? h("button", { type: "button", class: "primary small", onclick: applyNow }, "Apply now")
      : h("span", { class: "muted" }, "Restart the server to apply them."));
  bar.hidden = false;
}
async function applyNow(e) {
  const bar = $("restartBar");
  e.target.disabled = true;
  try { await api("restart", { method: "POST" }); } catch (err) { e.target.disabled = false; fail(err); return; }
  bar.textContent = "";
  bar.append(h("span", { class: "grow" }, "Restarting to apply your changes. This usually takes under a minute; chats pick up where they left off."));
  await sleep(3000);
  for (let i = 0; i < 60; i++) {
    try {
      const a = await api("app");
      if (!a.restart?.pending?.length) {
        toast("Changes applied");
        boot();
        return;
      }
    } catch {}
    await sleep(2000);
  }
  bar.textContent = "";
  bar.append(h("span", { class: "grow" }, "The server has not come back yet. Reload this page in a minute; if it still fails, check the server's log."));
}

// Team: every agent, what it does, its model, tools, where it answers and
// what it may spend. Saving edits config.yaml; the bar above applies it.

const TOOLSET_INFO = {
  core: ["Files, commands and documents", "Read and write files, run commands, read PDFs and images, send files"],
  memory: ["Memory", "Remember things between chats"],
  skills: ["Skills", "Follow saved routines the same way every time"],
  schedule: ["Schedules", "Set up repeating tasks and reminders"],
  delegate: ["Hand off work", "Ask another agent to do a task"],
  board: ["Task board", "Create and follow tasks for the team"],
  browser: ["Web browser", "Open websites, fill in forms, take screenshots"],
  computer: ["Computer use", "Use desktop apps with the mouse and keyboard"],
  meeting: ["Meetings", "Join video calls and take notes"],
  homeassistant: ["Home Assistant", "Control smart home devices"],
  a2a: ["Other agents", "Work with agents outside this install"],
};
function toolsetInfo(ts) {
  if (TOOLSET_INFO[ts]) return TOOLSET_INFO[ts];
  if (ts.startsWith("mcp:")) return [ts.slice(4), "A connected service and its tools"];
  return [ts, ""];
}
function placeLabel(p) {
  if (!p.conversation) return `everything else on ${p.channel}`;
  return `${p.channel}: ${p.label || p.conversation}`;
}
let modelChoices = null;
async function modelOptions() {
  if (!modelChoices) {
    modelChoices = h("datalist", { id: "modelChoices" });
    document.body.append(modelChoices);
    await sleep(0);
    try {
      const j = await api("models");
      for (const p of j.providers) for (const m of p.models) modelChoices.append(h("option", { value: `${p.provider}/${m}` }));
    } catch {}
  }
  return modelChoices;
}
// chipList edits a list of strings as removable chips plus an input.
function chipList(values, placeholder, list) {
  let vals = values.slice();
  const box = h("div", { class: "chips" });
  const input = h("input", { type: "text", placeholder, list, "aria-label": placeholder, style: "min-width:220px" });
  const draw = () => {
    box.textContent = "";
    vals.forEach((v, i) => box.append(h("span", { class: "chip mono" }, v,
      h("button", { type: "button", "aria-label": "Remove " + v, onclick: () => { vals.splice(i, 1); draw(); } }, "×"))));
    box.append(input);
  };
  const add = () => { const v = input.value.trim(); if (v && !vals.includes(v)) vals.push(v); input.value = ""; draw(); input.focus(); };
  input.addEventListener("keydown", e => { if (e.key === "Enter") { e.preventDefault(); add(); } });
  input.addEventListener("change", add);
  draw();
  return { el: box, get: () => vals.slice() };
}

pages.team = async view => {
  loading(view);
  let j;
  try { j = await api("team"); } catch (err) { view.textContent = ""; view.append(h("div", { class: "empty err" }, err.message)); return; }
  modelOptions(); // fills the model suggestions when the providers answer
  view.textContent = "";
  view.append(h("p", { class: "muted" },
    "Each agent below has its own job, model and tools. Change what you need and press Save; changes to models, tools and where they post take effect after the restart the bar at the top offers."));
  for (const a of j.agents) view.append(agentCard(a, j));
};

function agentCard(a, j) {
  const desc = h("input", { type: "text", value: a.description, placeholder: "What this agent does, in a sentence", disabled: !j.writable });
  const model = h("input", { type: "text", value: a.model, list: "modelChoices", class: "mono", disabled: !j.writable, "aria-label": "Main model" });
  const backups = chipList(a.fallbacks, "Add a backup model", "modelChoices");
  const boxes = {};
  const checks = h("div", { class: "checks" }, j.toolsets.map(ts => {
    const [title, help] = toolsetInfo(ts);
    boxes[ts] = h("input", { type: "checkbox", checked: a.toolsets.includes(ts), disabled: !j.writable });
    return h("label", {}, boxes[ts], h("span", {}, title, help ? h("small", {}, help) : null));
  }));
  const home = h("input", { type: "text", value: a.home || "", class: "mono", placeholder: "CHANNEL:CHAT, e.g. slack:T0123/C0456", disabled: !j.writable });
  const report = h("select", { disabled: !j.writable, "aria-label": "Post automatically" },
    [["", "Results (default)"], ["results", "Results"], ["all", "When it starts and when it finishes"], ["off", "Nothing automatic"]]
      .map(([v, t]) => h("option", { value: v, selected: (a.report_home || "") === v }, t)));
  const daily = h("input", { type: "number", min: "0", step: "0.5", value: a.daily || "", placeholder: "no limit", style: "max-width:120px" });
  const monthly = h("input", { type: "number", min: "0", step: "1", value: a.monthly || "", placeholder: "no limit", style: "max-width:120px" });
  const save = h("button", { type: "submit", class: "primary", disabled: !j.writable }, "Save");
  const f = h("form", { class: "panel agentcard" },
    h("div", { class: "row" }, h("h2", { class: "grow" }, a.name),
      a.permission ? badge(a.permission === "full" ? "full access" : a.permission, a.permission === "full" ? "warn" : "") : null),
    h("div", { class: "desc" }, a.description || h("span", { class: "muted" }, "No description yet")),
    h("div", { class: "small muted" }, "Answers in"),
    h("div", { class: "chips", style: "margin:4px 0 12px" }, a.answers.length ? a.answers.map(p => h("span", { class: "chip" }, placeLabel(p)))
      : h("span", { class: "muted small" }, "No chat yet. Other agents can still hand it work; use Who answers where to give it a channel.")),
    h("div", { class: "twocol" }, field("Main model", model), h("label", { class: "field" }, "Backup models, tried in order", backups.el)),
    h("div", { class: "small muted", style: "margin-top:10px" }, "What it can do"),
    checks,
    a.default_toolsets ? h("div", { class: "small muted" }, "It has the standard set. Changing a box saves an exact list for this agent.") : null,
    h("div", { class: "twocol" }, field("Spending limit per day (US$)", daily), field("Spending limit per month (US$)", monthly)),
    h("details", { class: "more" }, h("summary", {}, "Description and where it posts updates"),
      field("Description", desc), h("div", { class: "twocol" }, field("Posts its updates and questions to", home), field("Posts automatically", report))),
    h("div", { class: "row end", style: "margin-top:10px" }, save));
  f.addEventListener("submit", async e => {
    e.preventDefault();
    const toolsets = j.toolsets.filter(ts => boxes[ts].checked);
    if (!toolsets.includes("core") && !confirm(`Without "Files, commands and documents", ${a.name} can only talk. Save anyway?`)) return;
    const body = {
      description: desc.value, model: model.value.trim(), fallbacks: backups.get(), home: home.value.trim(), report_home: report.value,
      daily: +daily.value || 0, monthly: +monthly.value || 0,
    };
    const same = toolsets.length === a.toolsets.length && toolsets.every(t => a.toolsets.includes(t));
    if (!same) body.toolsets = toolsets;
    save.disabled = true;
    try {
      const r = await api("agents/" + encodeURIComponent(a.name), { method: "PUT", body });
      toast(`Saved ${a.name}`);
      showRestart(r);
    } catch (err) { fail(err); } finally { save.disabled = false; }
  });
  return f;
}

// Who answers where: each channel's default agent, and the chats sent to
// someone else. Labels are names for people; they are kept in config.yaml
// next to each chat.

pages.routing = async view => {
  loading(view);
  let j;
  try { j = await api("routing"); } catch (err) { view.textContent = ""; view.append(h("div", { class: "empty err" }, err.message)); return; }
  view.textContent = "";
  view.append(h("p", { class: "muted" },
    "Pick which agent answers in each place. A channel's main agent answers everywhere on it, except the chats you give to someone else below. A chat is its id on that platform (in Slack, the workspace id, a slash and the channel id: T0123/C0456)."));
  if (!j.channels.length) view.append(emptyState("No chat channels yet. Run hk setup channel in a terminal to add WhatsApp, Telegram, Slack and more."));
  for (const c of j.channels) view.append(routingPanel(c, j));
};

function routingPanel(c, j) {
  const pickAgent = (value, label) => {
    const sel = h("select", { "aria-label": label, disabled: !j.writable });
    for (const a of j.agents) sel.append(h("option", { value: a, selected: a === value }, a));
    sel.value = value;
    return sel;
  };
  const main = pickAgent(c.agent, `Main agent for ${c.account}`);
  const rows = [];
  const tbody = h("tbody");
  const addRow = r => {
    const row = {
      label: h("input", { type: "text", value: r.label || "", placeholder: "#sales", "aria-label": "Name", disabled: !j.writable }),
      conv: h("input", { type: "text", value: r.conversation || "", placeholder: "chat id", class: "mono", "aria-label": "Chat id", disabled: !j.writable }),
      agent: pickAgent(r.agent || c.agent, "Agent"),
    };
    row.tr = h("tr", {}, h("td", {}, row.label), h("td", {}, row.conv), h("td", {}, row.agent),
      h("td", {}, h("button", { type: "button", class: "danger small", disabled: !j.writable, onclick: () => { row.tr.remove(); rows.splice(rows.indexOf(row), 1); } }, "Remove")));
    rows.push(row);
    tbody.append(row.tr);
  };
  c.routes.forEach(addRow);
  const save = h("button", { type: "submit", class: "primary", disabled: !j.writable }, "Save");
  const f = h("form", { class: "panel" },
    h("div", { class: "row" }, h("h2", { class: "grow" }, c.account), h("span", { class: "muted" }, c.type)),
    h("div", { class: "row" }, field("Main agent", main)),
    h("div", { class: "tablewrap" }, h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Chat"), h("th", {}, "Answered by"), h("th", {}))), tbody)),
    h("div", { class: "row end", style: "margin-top:10px" },
      h("button", { type: "button", class: "btn", disabled: !j.writable, onclick: () => addRow({}) }, "Add a chat"), save));
  f.addEventListener("submit", async e => {
    e.preventDefault();
    const routes = rows.map(r => ({ conversation: r.conv.value.trim(), agent: r.agent.value, label: r.label.value.trim() })).filter(r => r.conversation);
    save.disabled = true;
    try {
      const r = await api("routing/" + encodeURIComponent(c.account), { method: "PUT", body: { agent: main.value, routes } });
      toast(`Saved ${c.account}`);
      showRestart(r);
    } catch (err) { fail(err); } finally { save.disabled = false; }
  });
  return f;
}

// Pages that plugins add, listed under the menu.
function showExtensions() {
  const nav = $("extNav");
  nav.textContent = "";
  for (const e of app?.extensions || []) nav.append(h("a", { href: e.url }, icon(["M4 4h7v7H4z", "M13 4h7v7h-7z", "M4 13h7v7H4z", "M16.5 13v7", "M13 16.5h7"]), e.title));
}

// Menu icons: simple strokes, drawn here so the page needs nothing else.
const NAV_ICONS = {
  home: ["M3 11l9-8 9 8", "M5 10v10h14V10", "M10 20v-6h4v6"],
  overview: ["M3 12h4l3-8 4 16 3-8h4"],
  chat: ["M4 5h16v11H9l-5 4z"],
  team: ["M9 3.5a3.5 3.5 0 1 1 0 7a3.5 3.5 0 1 1 0-7z", "M2.5 20a6.5 6.5 0 0 1 13 0", "M16 4a3.5 3.5 0 0 1 0 6.5", "M21.5 20a6.5 6.5 0 0 0-4-6"],
  routing: ["M3 12h7l4-6h7", "M10 12l4 6h7", "M18 3l3 3-3 3", "M18 15l3 3-3 3"],
  sessions: ["M12 3a9 9 0 1 1 0 18a9 9 0 1 1 0-18z", "M12 7v5l3 2"],
  board: ["M4 4h6v16H4z", "M14 4h6v10h-6z"],
  schedules: ["M4 6h16v14H4z", "M4 10h16", "M8 3v4", "M16 3v4"],
  memory: ["M6 4h12v16l-6-4-6 4z"],
  skills: ["M13 3L5 14h6l-1 7 8-11h-6z"],
  usage: ["M4 20V11", "M10 20V5", "M16 20v-6", "M21 20H3"],
  nodes: ["M3 5h13v9H3z", "M1 18h17", "M19 8h3v11h-3z"],
  approvals: ["M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z", "M9 12l2 2 4-4"],
  config: ["M4 6h9", "M17 6h3", "M15 4v4", "M4 12h3", "M11 12h9", "M9 10v4", "M4 18h11", "M19 18h1", "M17 16v4"],
};
function navIcons() {
  for (const a of document.querySelectorAll("nav a[data-page]")) {
    if (a.querySelector("svg")) continue;
    const paths = NAV_ICONS[a.dataset.page];
    if (paths) a.prepend(icon(paths));
  }
}

// Home: the clock and greeting, what needs you, the team, routines,
// today's numbers and recent work, in a grid of cards.

function greeting(d) {
  const hr = d.getHours();
  return hr < 5 ? "Working late" : hr < 12 ? "Good morning" : hr < 17 ? "Good afternoon" : "Good evening";
}
function ringCard(pct, big, label) {
  const r = 34, c = 2 * Math.PI * r, p = Math.max(0, Math.min(1, pct || 0));
  return h("div", { class: "ring" },
    s("svg", { viewBox: "0 0 84 84", "aria-hidden": "true" },
      s("circle", { class: "track", cx: "42", cy: "42", r: String(r) }),
      s("circle", { class: "fill", cx: "42", cy: "42", r: String(r), "stroke-dasharray": String(c), "stroke-dashoffset": String(c * (1 - p)) })),
    h("div", { class: "metric" }, h("div", { class: "n" }, big), h("div", { class: "l" }, label)));
}
function initials(name) { return (name || "?").split(/[-_\s]+/).map(w => w[0]).join("").slice(0, 2).toUpperCase(); }
function cap(name) { return name ? name[0].toUpperCase() + name.slice(1) : ""; }

pages.home = view => {
  view.className = "home";
  const grid = h("div", { class: "bento" });
  view.append(grid);
  const assistant = cap(app?.default || "your agent");
  // Hero: clock, greeting, health and a box to ask the main agent.
  const clock = h("div", { class: "clock" });
  const date = h("div", { class: "greet" });
  const health = h("span", { class: "health" }, h("i"), "Checking…");
  const tickClock = () => {
    const d = new Date();
    let hh = d.getHours() % 12 || 12;
    clock.textContent = "";
    clock.append(`${hh}:${String(d.getMinutes()).padStart(2, "0")}`, h("small", {}, d.getHours() < 12 ? "am" : "pm"));
    date.textContent = `${greeting(d)}. ${d.toLocaleDateString(undefined, { weekday: "long", day: "numeric", month: "long" })}`;
  };
  tickClock();
  const clockTimer = setInterval(tickClock, 10000);
  onLeave(() => clearInterval(clockTimer));
  const q = h("input", { type: "text", placeholder: `Ask ${assistant} anything`, "aria-label": `Ask ${assistant}` });
  q.addEventListener("keydown", e => { if (e.key === "Enter" && q.value.trim()) ask(q.value.trim()); });
  grid.append(h("div", { class: "panel span-8" },
    h("div", { class: "hero" }, h("div", { class: "grow" }, date, clock), h("div", { style: "align-self:flex-start" }, health)),
    h("div", { class: "askbar" }, q, h("button", { type: "button", class: "primary", onclick: () => q.value.trim() && ask(q.value.trim()) }, "Ask"))));
  // Today: spend against the day's limit, or routines running.
  const today = h("div", { class: "panel span-4" }, h("div", { class: "cardhead" }, h("h3", {}, "Today"), h("a", { href: "#/usage" }, "Usage")));
  const todayBody = h("div");
  today.append(todayBody);
  grid.append(today);
  // Needs you, team, routines, recent work.
  const needs = h("div", { class: "lines needs" });
  grid.append(h("div", { class: "panel span-7" }, h("div", { class: "cardhead" }, h("h3", {}, "Needs you"), h("a", { href: "#/approvals" }, "All approvals")), needs));
  const team = h("div");
  grid.append(h("div", { class: "panel span-5" }, h("div", { class: "cardhead" }, h("h3", {}, "Your team"), h("a", { href: "#/team" }, "Manage")), team));
  const routines = h("div", { class: "lines" });
  grid.append(h("div", { class: "panel span-7" }, h("div", { class: "cardhead" }, h("h3", {}, "Routines"), h("a", { href: "#/schedules" }, "All routines")), routines));
  const recent = h("div", { class: "lines" });
  grid.append(h("div", { class: "panel span-5" }, h("div", { class: "cardhead" }, h("h3", {}, "Recent work"), h("a", { href: "#/sessions" }, "History")), recent));

  const load = async () => {
    const [o, ap, tm, sc, us, bu, se] = await Promise.all(["overview", "approvals", "team", "schedules", "usage?days=1", "budget", "sessions"]
      .map(p => api(p).catch(() => null)));
    if (o) {
      const down = o.channels.filter(c => c.state !== "connected");
      health.className = "health" + (down.length || o.errors.length ? " bad" : "");
      health.textContent = "";
      health.append(h("i"), down.length ? `${down.map(c => c.account).join(", ")} not connected`
        : o.errors.length ? `${o.errors.length} recent problem${o.errors.length > 1 ? "s" : ""}` : `All ${o.channels.length} channels connected`);
      health.onclick = () => go("#/overview");
      health.style.cursor = "pointer";
    }
    // Today
    todayBody.textContent = "";
    const spent = us?.total?.cost || 0;
    const limit = bu?.install?.today?.limit || 0;
    const on = o?.schedules?.enabled || 0, all = o?.schedules?.total || 0;
    todayBody.append(limit ? ringCard(spent / limit, money(spent), `of ${money(limit)} today`) : ringCard(all ? on / all : 0, `${on}/${all}`, "routines on"),
      h("div", { class: "row", style: "margin-top:16px;gap:22px;flex-wrap:nowrap" },
        h("div", { class: "metric" }, h("div", { class: "n" }, limit ? `${on}/${all}` : money(spent)), h("div", { class: "l" }, limit ? "routines on" : "spent today")),
        h("div", { class: "metric" }, h("div", { class: "n" }, String(us?.total?.calls || 0)), h("div", { class: "l" }, "model calls")),
        h("div", { class: "metric" }, h("div", { class: "n" }, String(o?.open_cards || 0)), h("div", { class: "l" }, "open tasks"))));
    // Needs you
    needs.textContent = "";
    const list = ap?.approvals || [];
    setApprovalCount(list.length);
    if (!list.length) needs.append(h("div", {}, h("span", { class: "what sub" }, "Nothing is waiting for you.")));
    for (const a of list.slice(0, 5)) {
      const decide = async ok => {
        try { await api("approvals/" + a.id, { method: "POST", body: { approve: ok } }); toast(ok ? "Approved" : "Denied"); load(); } catch (err) { fail(err); }
      };
      needs.append(h("div", {},
        h("div", { class: "what" }, h("div", { class: "q" }, String(a.summary || a.tool).split("\n")[0]), h("div", { class: "sub" }, `${a.agent || ""} · waiting ${ago(a.since).replace(" ago", "")}`)),
        h("button", { type: "button", class: "btn small", onclick: () => decide(false) }, "No"),
        h("button", { type: "button", class: "primary small", onclick: () => decide(true) }, "Yes")));
    }
    // Team
    team.textContent = "";
    const busy = new Set((o?.running || []).map(r => r.agent));
    const spend = Object.fromEntries((us?.by_agent || []).map(r => [r.key, r.cost]));
    for (const a of tm?.agents || []) {
      team.append(h("div", { class: "member" }, h("div", { class: "avatar" }, initials(a.name)),
        h("div", { style: "min-width:0" }, h("div", { class: "row", style: "gap:6px" }, h("strong", {}, cap(a.name)), h("span", { class: "dotstate " + (busy.has(a.name) ? "busy" : "ok") })),
          h("div", { class: "sub small muted", style: "white-space:nowrap;overflow:hidden;text-overflow:ellipsis" }, busy.has(a.name) ? "Working now" : (a.description || "Ready"))),
        h("span", { class: "small muted" }, money(spend[a.name] || 0))));
    }
    if (!tm?.agents?.length) team.append(h("div", { class: "muted small" }, "No agents yet."));
    // Routines
    routines.textContent = "";
    const items = (sc?.schedules || []).slice().sort((x, y) => (y.enabled - x.enabled) || String(x.next_run).localeCompare(String(y.next_run))).slice(0, 6);
    if (!items.length) routines.append(h("div", {}, h("span", { class: "what sub" }, "No routines yet. Ask in plain words, for example: every weekday at 8am, brief me.")));
    for (const r of items) {
      const t = h("button", { type: "button", class: "toggle" + (r.enabled ? " on" : ""), "aria-label": (r.enabled ? "Pause " : "Resume ") + r.name, title: r.enabled ? "On" : "Paused" });
      t.addEventListener("click", async () => {
        try { await api(`schedules/${r.id}/${r.enabled ? "pause" : "resume"}`, { method: "POST" }); r.enabled = !r.enabled; t.classList.toggle("on", r.enabled); toast(r.enabled ? "Resumed" : "Paused"); }
        catch (err) { fail(err); }
      });
      routines.append(h("div", {}, h("div", { class: "what" }, h("div", {}, r.name), h("div", { class: "sub" }, plainWhen(r.spec) + (r.last_status && !/^ok/.test(r.last_status) ? " · last run: " + r.last_status : ""))), t));
    }
    // Recent work
    recent.textContent = "";
    const ss = (se?.sessions || []).slice(0, 6);
    if (!ss.length) recent.append(h("div", {}, h("span", { class: "what sub" }, "Nothing yet today.")));
    for (const x of ss) recent.append(h("div", { class: "click", onclick: () => go("#/sessions/" + x.id) },
      h("span", { class: "avatar", style: "width:26px;height:26px;border-radius:8px;font-size:11px" }, initials(x.agent)),
      h("div", { class: "what" }, h("div", {}, x.title || "Untitled chat"), h("div", { class: "sub" }, cap(x.agent))), h("span", { class: "sub" }, ago(x.updated_at))));
  };
  every(10000, load);
};

// Start

let poll = 0;
async function boot() {
  try {
    app = await api("app");
  } catch (err) {
    if (!$("gate").open) { $("view").textContent = ""; $("view").append(h("div", { class: "empty err" }, err.message)); }
    return;
  }
  clearInterval(poll);
  showRestart(app.restart);
  showExtensions();
  navIcons();
  const tick = async () => {
    if (document.hidden) return;
    try { setApprovalCount((await api("approvals")).approvals.length); $("dot").className = "dot on"; }
    catch { $("dot").className = "dot"; }
  };
  tick();
  poll = setInterval(tick, 10000);
  route();
}
applyTheme();
if ("serviceWorker" in navigator) navigator.serviceWorker.register("sw.js", { scope: "./" }).catch(() => {});
boot();
