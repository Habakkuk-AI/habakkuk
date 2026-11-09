---
title: Computer use
description: "Let agents see a desktop through screenshots and drive it with the mouse and keyboard: the Linux, macOS and private virtual desktop backends, and the safety rules."
order: 3
---

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

Other settings: `display` (the X display for `linux`), `native` (see below) and `timeout`.

> [!NOTE]
> Computer use is not supported on Windows yet. Use the [browser](browser.md) tools, or run Habakkuk in WSL.

## The tools

| Tool | What it does |
|---|---|
| `computer_screenshot` | The model sees the screen, scaled to at most 1366 pixels a side; coordinates map back to the screen |
| `computer_click`, `computer_move`, `computer_drag` | Mouse actions |
| `computer_type`, `computer_key` | Type text, press keys (`ctrl+l`, `Return`) |
| `computer_scroll`, `computer_wait` | Scroll, wait |
| `computer_open` | Open an app or a URL |

When every model in the agent's chain is a Claude 5 or later model on an `anthropic` provider, the agent gets the provider's native computer-use tool instead (`native: false` turns that off). `computer_open` stays.

## Backends

```bash tab="linux"
# Drives $DISPLAY (or display) with xdotool; screenshots with ImageMagick's import, scrot or ffmpeg.
sudo apt install xdotool imagemagick
# On Wayland it uses grim and ydotool instead (start ydotoold).
```

```bash tab="macos"
# Uses screencapture and osascript (cliclick when installed).
brew install cliclick   # optional
# Then grant the app Habakkuk runs in Screen Recording and Accessibility
# in System Settings > Privacy & Security.
```

```bash tab="xvfb"
# A private virtual desktop per agent, started on its first action.
sudo apt install xvfb xdotool imagemagick openbox
habakkuk computer view   # prints the x11vnc (and noVNC) command to watch or take over
```

With `xvfb`, each agent gets a display of its own from `:99` up, readable only with a cookie Habakkuk writes, plus a window manager (`openbox` and others) when one is installed. `habakkuk doctor` checks the commands the backend needs.

## Safety

- Every computer action is `exec` risk: `ask` and `workspace` modes ask first unless an approval reviewer allows it. A turn may take at most `max_actions` actions.
- Text that looks like an API key, token, private key, payment card number or one of Habakkuk's configured secrets is never typed, whoever approves.
- Actions while an app in `confirm_apps` is in front, or opening one, always need a human, even in `full` mode.
- Screenshots count as untrusted content, like web pages.
- Agents whose commands run in a Docker or SSH sandbox cannot use the computer, which is on this machine.

[Architecture: computer](../reference/architecture.md#computer) has the details.
