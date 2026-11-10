---
title: Browser
description: "How agents use a real web browser, a headless Chrome Habakkuk starts or your own signed-in Chrome, with the tools, URL rules, server setup and safety."
order: 2
---

Use this page when you want an agent to work in a real web browser. Add the `browser` toolset to the agent, and it can open pages, read them, fill in forms, click, take screenshots and PDFs, and download files.

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

The URL settings take patterns. A pattern that contains `://` is matched against the whole URL. Any other pattern is matched against the host (or `host:port`). `*` matches anything, and `*.example.com` also matches `example.com`. There's one more setting, `args`, which adds command-line flags for the browser.

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

These tools count as `network` risk, so `workspace` mode lets them run without asking. The exception is `browser_evaluate`. It counts as `exec` and needs approval.

Each agent has its own browser. It starts on the agent's first browser call and keeps a profile in `$HABAKKUK_HOME/browser/<agent>`, so sign-ins persist. If the browser crashes, the next call replaces it.

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

Habakkuk looks on `PATH` for `chromium`, `chromium-browser`, `google-chrome`, `google-chrome-stable`, `chrome` or `chrome-headless-shell`. It also finds the binary that `path` names. On Windows it finds Chrome, Chromium or Edge in their usual install folders. Headless mode doesn't need a display. When Habakkuk runs as root, as it does in most containers, it adds `--no-sandbox` itself.

## Your own signed-in Chrome

Set `mode: connect` and Habakkuk works in tabs of a Chrome that you started yourself. That way it can use the sites you're signed in to. Start Chrome with remote debugging, then point Habakkuk at it:

```bash
# Chrome needs a separate profile directory to allow remote debugging.
google-chrome --remote-debugging-port=9222 --user-data-dir="$HOME/.config/habakkuk-chrome"
```

```yaml
browser:
  mode: connect
  cdp_url: http://127.0.0.1:9222
```

Sign in once, in that window, to the sites you want. Habakkuk opens its own tabs there and can switch to yours. When it goes idle, it closes only the tabs it opened.

> [!WARNING]
> Anything that can reach the debugging port has full control of that browser, so keep the port on loopback. An agent working in your signed-in browser can do whatever you can do there. Consider `permission: ask` or an approval reviewer for that agent.

## Safety

- The URL guard checks every request a driven tab makes. That includes redirects, frames, scripts, popups and the workers the tab starts. A page can't steer the browser to `localhost`, your LAN or a cloud metadata address unless `allow_private` says so. The guard doesn't cover WebSocket connections or service workers.
- The model receives snapshots, script results and tab titles marked as untrusted page content. They're never passed to it as instructions, and they taint the turn.
- A page on a private address that `allow_private` lets through counts as a risky capability. In a chat, it needs your approval.

For the details, see [Architecture: browser](../reference/architecture.md#browser).
