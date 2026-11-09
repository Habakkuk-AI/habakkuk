---
title: Browser and computer use
description: Let agents drive a real web browser (headless or your own signed-in Chrome) and see and control a desktop through screenshots, mouse and keyboard.
order: 12
---

## Browser

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

A pattern with `://` matches the whole URL, otherwise the host (or `host:port`); `*` matches anything, and `*.example.com` also matches `example.com`.

The tools: `browser_navigate`, `browser_snapshot` (the page as a compact accessibility tree, with numbered refs on links, fields and buttons), `browser_click`, `browser_type`, `browser_select`, `browser_press`, `browser_scroll`, `browser_back`, `browser_wait`, `browser_tabs`, `browser_screenshot` (the model sees the image; a PNG is saved in the workspace), `browser_pdf`, `browser_download` and `browser_evaluate` (JavaScript in the page). They are `network` risk, which `workspace` mode allows without asking, except `browser_evaluate`, which is `exec` and needs approval. Each agent has its own browser, started on its first browser call, with a profile in `$HABAKKUK_HOME/browser/<agent>` so sign-ins persist; a browser that crashes is replaced on the next call.

**Installing a browser on a server.** On Debian, `apt install chromium` (Ubuntu's `chromium` package needs snap; Google Chrome's `.deb` works too). Habakkuk finds `chromium`, `chromium-browser`, `google-chrome`, `google-chrome-stable`, `chrome` or `chrome-headless-shell` on `PATH`, or the binary `path` names. No display is needed in headless mode, and as root (as in most containers) Habakkuk adds `--no-sandbox` itself. For the container image, build the variant with Chromium:

```bash
docker build --target browser -t habakkuk:browser .
```

**Your own signed-in Chrome.** With `mode: connect`, Habakkuk works in tabs of a Chrome you started, so it can use the sites you are signed in to:

```bash
# Chrome needs a separate profile directory to allow remote debugging.
google-chrome --remote-debugging-port=9222 --user-data-dir="$HOME/.config/habakkuk-chrome"
```

```yaml
browser:
  mode: connect
  cdp_url: http://127.0.0.1:9222
```

Sign in to the sites you want in that window once. Habakkuk opens its own tabs there, can switch to yours, and when it goes idle it closes only the tabs it opened. The debugging port gives full control of that browser to anything that can reach it, so keep it on loopback. An agent acting in your signed-in browser can do whatever you can do there; consider `permission: ask` or an approval reviewer for it.

**Safety.** Every request a driven tab makes, including redirects, frames, scripts, popups and the workers it starts, goes through the URL guard, so a page cannot steer the browser to `localhost`, your LAN or a cloud metadata address unless `allow_private` says so. Not covered: WebSocket connections and service workers. Snapshots, script results and tab titles reach the model marked as untrusted page content, never as instructions.

[Architecture: browser](../reference/architecture.md#browser) has the details.

## Computer

Agents that list the toolset `computer` see a desktop through screenshots and drive it with the mouse and keyboard: any app, not just web pages.

```yaml
agents:
  habakkuk:
    toolsets: [core, memory, skills, computer]
computer:
  backend: xvfb                 # linux (this machine's screen) | macos | xvfb (a private virtual desktop per agent)
  screen: 1280x800              # xvfb only
  max_actions: 150              # per turn; -1 for no cap
  confirm_apps: ["1Password", "Terminal"]   # always ask a human first
```

The tools: `computer_screenshot` (the model sees it, scaled to at most 1366 pixels a side; coordinates map back to the screen), `computer_click`, `computer_move`, `computer_drag`, `computer_type`, `computer_key` (`ctrl+l`, `Return`), `computer_scroll`, `computer_wait` and `computer_open` (an app or a URL). When every model in the agent's chain is a Claude 5 or later model on an `anthropic` provider, the agent gets the provider's native computer-use tool instead (`native: false` turns that off); `computer_open` stays.

- **linux** drives `$DISPLAY` (or `display`) with `xdotool`, and takes screenshots with ImageMagick's `import`, `scrot` or `ffmpeg`. On Wayland it uses `grim` and `ydotool` (start `ydotoold`).
- **macos** uses `screencapture` and `osascript` (`cliclick` when installed). Grant the app Habakkuk runs in Screen Recording and Accessibility in System Settings > Privacy & Security.
- **xvfb** starts `Xvfb` for each agent on its first action, on a display of its own from `:99` up, readable only with a cookie Habakkuk writes, plus a window manager (`openbox` and others) when one is installed. On Debian: `apt install xvfb xdotool imagemagick openbox`. `habakkuk computer view` prints the `x11vnc` (and noVNC) command to watch or take over.

**Safety.** Every computer action is `exec` risk: `ask` and `workspace` modes ask first unless an approval reviewer allows it. A turn may take `max_actions` actions. Text that looks like an API key, token, private key, payment card number or one of Habakkuk's configured secrets is never typed, whoever approves. Actions while an app in `confirm_apps` is in front, or opening one, always need a human, even in `full` mode. Screenshots count as untrusted content, like web pages. Agents whose commands run in a Docker or SSH sandbox cannot use the computer, which is on this machine. `habakkuk doctor` checks the commands the backend needs.

[Architecture: computer](../reference/architecture.md#computer) has the details.
