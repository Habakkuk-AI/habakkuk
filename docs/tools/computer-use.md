---
title: Computer use
description: "Let agents see a desktop through screenshots and drive it with the mouse and keyboard, on Linux, macOS or a private virtual desktop, within the safety rules."
order: 3
---

Use this page when an agent needs to work in desktop apps as well as web pages. An agent with the `computer` toolset sees a desktop through screenshots and drives it with the mouse and keyboard, so it can use any app.

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

There are three more settings: `display` (the X display for `linux`), `native` (explained below) and `timeout`.

> [!NOTE]
> Computer use doesn't work on Windows yet. Use the [browser](browser.md) tools there, or run Habakkuk in WSL.

## The tools

| Tool | What it does |
|---|---|
| `computer_screenshot` | The model sees the screen, scaled to at most 1366 pixels a side; coordinates map back to the screen |
| `computer_click`, `computer_move`, `computer_drag` | Mouse actions |
| `computer_type`, `computer_key` | Type text, press keys (`ctrl+l`, `Return`) |
| `computer_scroll`, `computer_wait` | Scroll, wait |
| `computer_open` | Open an app or a URL |

If every model in the agent's chain is a Claude 5 or later model on an `anthropic` provider, the agent gets the provider's native computer-use tool in place of these. Set `native: false` to turn that off. The agent keeps `computer_open` either way.

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
hk computer view         # prints the x11vnc (and noVNC) command to watch or take over
```

With `xvfb`, each agent gets its own display, numbered from `:99` up. The display can only be read with a cookie that Habakkuk writes. The agent also gets a window manager (`openbox` and others) when one is installed. `hk doctor` checks for the commands the backend needs.

## Safety

- Every computer action counts as `exec` risk. In `ask` and `workspace` modes the agent asks first, unless an approval reviewer allows the action. A turn can take at most `max_actions` actions.
- Habakkuk never types text that looks like an API key, token, private key, payment card number or one of its configured secrets. That holds whoever approves.
- A human always has to approve an action taken while an app in `confirm_apps` is in front, and the opening of one. This applies even in `full` mode.
- Screenshots are treated as untrusted content, the same as web pages.
- An agent whose commands run in a Docker or SSH sandbox can't use the computer, because the computer is on this machine.

For the details, see [Architecture: computer](../reference/architecture.md#computer).
