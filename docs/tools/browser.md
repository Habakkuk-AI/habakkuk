---
title: Browser
description: "Let agents drive a real web browser: a headless Chrome Habakkuk starts, or your own signed-in Chrome; the tools, URL rules, installing a browser on a server, and safety."
order: 2
---

Agents that list the toolset `browser` can use a real web browser: open pages, read them, fill in forms, click, take screenshots and PDFs, and download files.

```yaml
agents:
  habakkuk:
    toolsets: [core, memory, skills, browser]
browser:
  mode: launch                  # launch (default): Habakkuk starts a headless Chrome or Chromium
  # path: /usr/bin/chromium     # default: the first Chrome or Chromium on PATH or in a usual place
  headless: true
  allowed_urls: []              # when set, the only pages it may open
  blocked_urls: ["*.doubleclick.net", "https://example.com/admin*"]
  allow_private: ["localhost:3000"]   # loopback and private addresses are refused otherwise; ["*"] allows all
  idle_timeout: 10m             # close the browser after this long unused
  timeout: 60s                  # per tool call
```

URL patterns: a pattern with `://` matches the whole URL, otherwise the host (or `host:port`). `*` matches anything, and `*.example.com` also matches `example.com`. `args` adds command-line flags for the browser.

## The tools

| Tool | What it does |
|---|---|
| `browser_navigate`, `browser_back` | Open a URL, go back |
| `browser_snapshot` | The page as a compact accessibility tree, with numbered refs on links, fields and buttons |
| `browser_click`, `browser_type`, `browser_select`, `browser_press`, `browser_scroll` | Act on the page by ref |
| `browser_wait`, `browser_tabs` | Wait for the page; list and switch tabs |
| `browser_screenshot` | The model sees the image; a PNG is saved in the workspace |
| `browser_pdf`, `browser_download` | Save the page as PDF, download a file |
| `browser_evaluate` | Run JavaScript in the page |

They are `network` risk, which `workspace` mode allows without asking, except `browser_evaluate`, which is `exec` and needs approval. Each agent has its own browser, started on its first browser call, with a profile in `$HABAKKUK_HOME/browser/<agent>` so sign-ins persist. A browser that crashes is replaced on the next call.

## Installing a browser on a server

```bash tab="Debian"
apt install chromium
```

```bash tab="Ubuntu"
# Ubuntu's chromium package needs snap; Google Chrome's .deb works too
wget https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
apt install ./google-chrome-stable_current_amd64.deb
```

```bash tab="Docker"
docker build --target browser -t habakkuk:browser .
```

Habakkuk finds `chromium`, `chromium-browser`, `google-chrome`, `google-chrome-stable`, `chrome` or `chrome-headless-shell` on `PATH`, or the binary `path` names. On Windows it finds Chrome, Chromium or Edge in their usual install folders. No display is needed in headless mode, and as root (as in most containers) Habakkuk adds `--no-sandbox` itself.

## Your own signed-in Chrome

With `mode: connect`, Habakkuk works in tabs of a Chrome you started, so it can use the sites you are signed in to:

```bash
# Chrome needs a separate profile directory to allow remote debugging.
google-chrome --remote-debugging-port=9222 --user-data-dir="$HOME/.config/habakkuk-chrome"
```

```yaml
browser:
  mode: connect
  cdp_url: http://127.0.0.1:9222
```

Sign in to the sites you want in that window once. Habakkuk opens its own tabs there, can switch to yours, and when it goes idle it closes only the tabs it opened.

> [!WARNING]
> The debugging port gives full control of that browser to anything that can reach it, so keep it on loopback. An agent acting in your signed-in browser can do whatever you can do there; consider `permission: ask` or an approval reviewer for it.

## Safety

- Every request a driven tab makes, including redirects, frames, scripts, popups and the workers it starts, goes through the URL guard. A page cannot steer the browser to `localhost`, your LAN or a cloud metadata address unless `allow_private` says so. Not covered: WebSocket connections and service workers.
- Snapshots, script results and tab titles reach the model marked as untrusted page content, never as instructions, and they taint the turn.
- Pages on private addresses that `allow_private` lets through count as a risky capability: in a chat they need your approval.

[Architecture: browser](../reference/architecture.md#browser) has the details.
