"use strict";
// Quick ask: one small window that sends a message to the default agent in
// a web conversation of its own and shows the reply as it streams. Habakkuk's
// API is reached through the app's proxy (same origin), which adds the
// token.

const $ = id => document.getElementById(id);
let agent = "";
let conv = "";
let lastSeq = 0;
let ctrl = null;
let answer = null; // the element the current reply streams into

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}

async function post(path, body) {
  const res = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!res.ok) {
    const j = await res.json().catch(() => ({}));
    throw new Error(j.error || "HTTP " + res.status);
  }
  return res.status === 204 ? null : res.json().catch(() => null);
}

// stream reads a Server-Sent Events response until signal aborts,
// reconnecting after a break.
async function stream(path, onEvent, signal) {
  while (!signal.aborted) {
    try {
      const res = await fetch(path(), { signal });
      if (!res.ok) throw new Error("HTTP " + res.status);
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
          if (data && !signal.aborted) {
            try { onEvent(event, JSON.parse(data)); } catch {}
          }
        }
      }
    } catch {
      if (signal.aborted) return;
    }
    await new Promise(r => setTimeout(r, 1500));
  }
}

function reset() {
  if (ctrl) ctrl.abort();
  ctrl = new AbortController();
  conv = "quick-" + Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
  lastSeq = 0;
  answer = null;
  $("out").textContent = "";
  const sig = ctrl.signal;
  // The streamed text of the turn as it is written.
  stream(() => `/api/chat/${conv}/live`, (ev, d) => {
    if (ev !== "activity") return;
    if (!answer) answer = $("out").appendChild(el("p", "a", ""));
    if (d.kind === "text") {
      answer.dataset.live = "1";
      answer.textContent += d.text || "";
    } else if (d.kind === "tool_start") {
      $("out").insertBefore(el("div", "tool", "using " + d.tool + "…"), answer);
    } else if (d.kind === "turn_error") {
      answer.textContent += "\n[" + (d.text || "the turn failed") + "]";
    }
    scroll();
  }, sig);
  // The conversation's messages: the final reply replaces the streamed one.
  stream(() => `/api/chat/${conv}/events?after=${lastSeq}`, (ev, m) => {
    if (ev !== "message") return;
    lastSeq = Math.max(lastSeq, m.seq);
    if (m.role !== "assistant") return;
    if (!answer) answer = $("out").appendChild(el("p", "a", ""));
    answer.textContent = m.text;
    delete answer.dataset.live;
    answer = null;
    scroll();
  }, sig);
  post("/desktop/api/ask", { conversation: conv }).catch(() => {});
  $("q").focus();
}

function scroll() { const o = $("out"); o.scrollTop = o.scrollHeight; }

$("form").addEventListener("submit", async e => {
  e.preventDefault();
  const text = $("q").value.trim();
  if (!text) return;
  $("q").value = "";
  $("out").append(el("p", "q", text));
  answer = null;
  scroll();
  try {
    await post("/api/chat", { conversation: conv, text, agent });
  } catch (err) {
    $("out").append(el("p", "a err", err.message));
  }
});
$("q").addEventListener("keydown", e => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); $("form").requestSubmit(); }
});
$("newBtn").addEventListener("click", reset);
$("openBtn").addEventListener("click", () => post("/desktop/api/open", { path: "/app/#/chat/" + conv }).catch(() => {}));
// whoami learns the default agent; it runs again whenever the window comes
// back, so a connection made (or switched) meanwhile shows up.
async function whoami() {
  try {
    const res = await fetch("/api/app");
    if (!res.ok) throw new Error();
    const app = await res.json();
    agent = app.default || "";
    $("who").textContent = agent ? "Asking " + agent : "";
  } catch {
    agent = "";
    $("who").textContent = "Not connected";
  }
}
window.addEventListener("focus", () => { $("q").focus(); whoami(); });

whoami();
reset();
