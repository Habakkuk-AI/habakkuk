---
title: Desktop app
description: "Habakkuk Desktop: a native window around the dashboard with a tray icon, notifications, quick ask, start at login and a connection manager."
order: 16
---

Habakkuk Desktop is a native app for macOS, Windows and Linux around the dashboard every `habakkuk serve` already has at `/app/`. It adds what a browser tab cannot: a menu bar (tray) icon with status, notifications for approvals and replies with Approve and Deny buttons, a quick ask window on a global hotkey, starting at login, and running `habakkuk serve` for you.

It lives in [`apps/desktop`](../../apps/desktop), a Go module of its own built with [Wails v3](https://v3.wails.io). The `habakkuk` binary does not depend on it and still builds without CGO.

## How it works

```
 Habakkuk Desktop                                        a Habakkuk
 ┌──────────────────────────────────────────┐          ┌──────────────────────┐
 │ main window ─┐                           │          │ habakkuk serve         │
 │ quick ask ───┼─> http://127.0.0.1:PORT ──┼── proxy ─┼─> /app/  /api/       │
 │              │   /desktop/  app pages    │  +token  │                      │
 │ tray, hotkey,│   /desktop/api/  app API  │          │                      │
 │ notifications│                           │          └──────────────────────┘
 │ supervisor ──┼── starts and stops ───────┼────────> habakkuk serve (local)
 └──────────────────────────────────────────┘
```

- **One UI codebase.** The main window shows the connected Habakkuk's own dashboard, so it is always the version that Habakkuk ships. The app's windows load everything from a small web server on `127.0.0.1`, which passes `/app/` and `/api/` to the active Habakkuk with its token added. The token never reaches the page, and the page sees one origin whichever Habakkuk is connected. Over loopback the microphone works even when the Habakkuk itself is plain `http`.
- **Only the app can use that server.** It answers only requests addressed to its own loopback address, so DNS rebinding gets nowhere. It needs a session cookie that the app's windows get by opening a URL with a fresh random key each launch. The cookie is `HttpOnly` and `SameSite=Strict`, changes must be JSON, and requests a browser marks as cross-site are refused. Another local program can still open a connection to the port, but without the key it gets `403`.
- **The app's own pages** (connections, logs, settings, quick ask) are static files embedded in the app. The CSP is the dashboard's strict policy: only the app's own scripts, no inline code.
- **Notifications.** The app follows the event stream (`/api/chat/{conversation}/events`) of each web conversation active in the last day, up to eight at once, and notifies assistant replies newer than the app's start. It asks `/api/approvals` every 5 seconds and the overview every 15 seconds, or every second while Habakkuk does not answer. An approval prompt is only notified once, as an approval with buttons, even though it is also a chat message. Replies are not notified while the main window has focus, or while the quick ask window shows that conversation.

## Connections

A connection is either:

- **Local.** A Habakkuk on this computer, given by its `HABAKKUK_HOME` (default `$HABAKKUK_HOME` or `~/.habakkuk`). Its address and token come from that home, the way `habakkuk serve` reads them: `http.listen` and `http.token_env` in `config.yaml`, the token from the environment or `.env`. When you activate it, the app starts `habakkuk serve` as a child process with that `HABAKKUK_HOME` and a `PATH` that includes the usual install folders. It shows the output in the connections page, restarts it after a crash (1 second, doubling to 30), and stops it with SIGTERM (a kill after 10 seconds) when you pause it or quit. If `serve.json` shows a `habakkuk serve` already running there, the app connects to it and leaves it alone. Tick "only when I press Start" to never start it automatically.
- **Remote.** Any Habakkuk reachable over HTTP(S): a base URL (`https://habakkuk.example.com`, or with a path prefix behind a reverse proxy; a pasted `/app/` is dropped) and the token. The token is stored in the OS keychain: the login Keychain on macOS, the Secret Service (GNOME Keyring, KWallet) on Linux, the Credential Manager on Windows. It is filed under the service "Habakkuk Desktop" and never written to the config file. **Test** checks the URL and token against `/api/app` before you save.

The list, the active connection and the settings are in `config.json` in the OS's per-user config folder (`~/Library/Application Support/Habakkuk Desktop/` on macOS, `%AppData%\Habakkuk Desktop\` on Windows, `~/.config/Habakkuk Desktop/` on Linux), mode 0600. `HABAKKUK_DESKTOP_CONFIG` points somewhere else. The app's log is `desktop.log` in the same folder.

The habakkuk executable for a local connection is the one set on the connection, else the one bundled with the app (macOS and Linux packages include it), else `habakkuk` on `PATH`, else `~/.local/bin/habakkuk`, `/usr/local/bin/habakkuk` or `/opt/homebrew/bin/habakkuk`.

A local Habakkuk needs an `http` section in its `config.yaml`. Without one, `habakkuk serve` has no dashboard, and the connections page says so:

```yaml
http:
  listen: 127.0.0.1:8484
  token_env: HABAKKUK_HTTP_TOKEN   # optional on loopback; put the value in .env
```

## What you see

What follows describes the screens in words; there are no screenshots in the repository.

- **Main window.** The Habakkuk dashboard (overview, chat, sessions, board, schedules, memory, skills, usage, approvals, config), the same as in a browser. While a connection is coming up, the window shows the connections page with a "Connecting" status, then switches to the dashboard once Habakkuk answers. Closing the window keeps the app in the tray. On macOS the menu bar has File → New Chat (Cmd+N), Quick Ask, Connections… (Cmd+,), plus the usual Edit, View and Window menus.
- **Connections page** (`Connections…`, or the tray). A card for the active connection with a coloured dot (green connected, amber when a channel is down, red when not connected), its channel and approval counts, and any problem in a red box ("no Habakkuk is set up in ~/.habakkuk yet: run `habakkuk setup`", "config.yaml has no http section", "The token is missing or wrong."). For a local connection, a panel shows `habakkuk serve`: its state (running, backoff, stopped, or "running outside the app"), pid, address, restart count, last exit, Start and Stop buttons, and a scrolling log. Below that are the list of connections, each with a radio button to switch to it, a badge (local, remote, or "remote, no token"), and Edit and Remove links. The add/edit form shows URL and Token fields for a remote, and HABAKKUK_HOME, executable and "only when I press Start" for a local one. Last come the settings: the hotkey, start at login, and notifications on or off.
- **Tray / menu bar.** On macOS, the Habakkuk mark (two tablets forming an H, with the light above) as a template image, which macOS tints to match light and dark menu bars. A variant with a dot in the corner shows while approvals are waiting, the connection has a problem, or Habakkuk is not answering, and the number of waiting approvals shows next to it. On Windows and Linux the tray shows the app icon. The menu lists the status ("Connected to Home"), "Channels: 3 of 3 connected", "Approvals waiting: 2" (opens the approvals page), then Open Habakkuk, New Chat, Quick Ask (with its hotkey), a Connections submenu with one radio item per connection plus "Manage Connections…", "Pause habakkuk serve" or "Resume habakkuk serve" for a local connection, a "Start at Login" checkbox, and Quit Habakkuk.
- **Quick ask.** Cmd+Shift+Space on macOS, Ctrl+Shift+Space elsewhere, opens a small window that floats on top in the middle of the screen. It has a text box and Send. The reply streams in under your question as the model writes it, with a grey "using exec…" line for each tool call. The final message replaces the streamed text when it arrives. Its footer says which agent you are asking, with New (a fresh conversation) and Open in Habakkuk (the same conversation in the main window's chat). Escape or clicking elsewhere hides it. Each quick ask conversation is an ordinary web conversation (`quick-...`), so it is also in the dashboard's chat list and in sessions.
- **Notifications.** "Approval needed: exec" with the tool's summary and **Approve** and **Deny** buttons. Clicking the notification itself opens the approvals page. A reply shows as "Habakkuk replied" (or the agent's name) with the first 240 characters, and clicking it opens that conversation.

## Building

You need Go (the version in `apps/desktop/go.mod`) with CGO and the platform's webview:

- **macOS** 11 or later: Xcode or the Command Line Tools.
- **Linux**: GTK 4 and WebKitGTK 6, e.g. `sudo apt install libgtk-4-dev libwebkitgtk-6.0-dev` on Ubuntu 24.04.
- **Windows** 10 or 11: the WebView2 runtime (preinstalled on Windows 11). No C compiler is needed.

```bash
cd apps/desktop
go test ./...
go build -o habakkuk-desktop .          # a plain binary; run it from a terminal
HABAKKUK_DESKTOP_DEBUG=1 ./habakkuk-desktop   # also logs every request to the app's local server
```

Packages, as the release workflow makes them:

```bash
VERSION=v0.3.0 apps/desktop/scripts/package.sh darwin    # dist/Habakkuk.app (universal, bundles habakkuk), .zip and .dmg
VERSION=v0.3.0 apps/desktop/scripts/package.sh linux     # dist/Habakkuk-Desktop_v0.3.0_linux_amd64.tar.gz
VERSION=v0.3.0 apps/desktop/scripts/package.sh windows   # dist/Habakkuk-Desktop_v0.3.0_windows_amd64.exe (Git Bash)
```

Wails v3 apps are ordinary Go programs, so the script calls `go build` directly, with `lipo`, `iconutil`, `codesign` and `hdiutil` on macOS. It does not use the `wails3` Taskfile scaffolding. The icons come from the Habakkuk app icon (`assets/brand/app-icon-1024.png`, embedded in `internal/icon` with the menu bar template images). `go run ./cmd/icons` writes the files each package needs: the macOS `.icns` (through `iconutil`), the Windows `.ico` linked into the executable together with the application manifest (`packaging/windows/habakkuk-desktop.exe.manifest`) and version info, and the Linux PNG. `packaging/` also holds the macOS `Info.plist` and the Linux `.desktop` file. The macOS bundle ID is `ai.habakkuk.desktop`. On Linux, install by unpacking the archive, putting `habakkuk-desktop` and `habakkuk` on your `PATH`, and copying `habakkuk-desktop.desktop` to `~/.local/share/applications/` and `habakkuk-desktop.png` to `~/.local/share/icons/`.

The release workflow (`.github/workflows/release.yml`, job `desktop`) builds all three on tag pushes, after the `binaries` job has created the release, and attaches the files to it.

## Unsigned builds

The release builds are not signed with a developer certificate or notarized:

- **macOS** has an ad-hoc signature only. The first time, open it with right-click → Open, or run `xattr -dr com.apple.quarantine /Applications/Habakkuk.app`. macOS asks once whether Habakkuk may send notifications, and asks again after upgrading from a build with an older bundle ID, since it treats `ai.habakkuk.desktop` as a new app. Start at login uses `SMAppService` on macOS 13 and later (System Settings → General → Login Items shows it), a LaunchAgent before that.
- **Windows** SmartScreen warns about an unknown publisher: More info → Run anyway.
- **Linux** has nothing to sign.

## Limits

- **Notifications cover the web chat only.** Replies on Telegram, Slack and the other channels are not notified, since you get them there already. Approvals are notified wherever they wait. Only the eight most recently active web conversations of the last day are followed.
- **Notification buttons.** Approve and Deny appear on macOS (in the bundled app) and Windows, and on Linux where the notification server supports actions. A plain `go build` binary on macOS is not a bundle, so macOS refuses it native notifications; the app falls back to `osascript` banners without buttons. The fallback on Linux is `notify-send`.
- **Global hotkey.** On Linux it needs X11, or a Wayland compositor with the GlobalShortcuts portal; elsewhere it may not register. The settings page reports a hotkey the system refused and keeps the old one.
- **Windows** has no bundled `habakkuk`, because the main binary does not build for Windows yet. Use remote connections, or point a local connection at a `habakkuk.exe` you built. Stopping a local serve there kills it rather than asking it to stop.
- **"New Chat"** opens the dashboard's chat and presses its New chat button, since the dashboard has no address for a new chat. **"Open in Habakkuk"** from quick ask opens `#/chat/<conversation>`; the dashboard selects it once it has merged the server's conversation list.
- **Quitting the app stops the `habakkuk serve` it started.** For an agent that must stay up (channels, schedules), run `habakkuk serve` as a service and let the app connect to it; the app never stops a serve it did not start.
- The dashboard's own settings (theme, chat list) are kept per origin, so the app keeps its loopback port across launches. If that port is taken, it picks another and they start afresh.
- Not tested in this repository: the Windows and Linux builds (CI builds them), notification buttons, the global hotkey and start at login on each OS. Tests cover the connection manager, the event stream client and notifier, the loopback server and proxy, and the supervisor with a fake `habakkuk` binary.
