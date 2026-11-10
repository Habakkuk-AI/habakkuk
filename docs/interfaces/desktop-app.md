---
title: Desktop app
description: "Habakkuk Desktop puts the dashboard in a native window and adds a tray icon, notifications, quick ask, start at login and a connection manager."
order: 3
---


Habakkuk Desktop is a native app for macOS, Windows and Linux. It wraps the dashboard that every `hk serve` already has at `/app/`, and adds what a browser tab can't do. You get a menu bar (tray) icon that shows status. You get notifications for approvals and replies, with Approve and Deny buttons. A global hotkey opens a quick ask window. The app can start at login, and it can run `hk serve` for you.

This page covers installing the app, how it connects to a Habakkuk, what each screen shows, building it yourself, and its limits.

The app's code is in [`apps/desktop`](../../apps/desktop). It's a Go module of its own, built with [Wails v3](https://v3.wails.io). The `habakkuk` binary doesn't depend on it, and still builds without CGO.

## Install

Download the build for your OS from the [latest release](https://github.com/habakkuk-ai/habakkuk/releases/latest). For macOS it's `Habakkuk.app` in a `.zip` or `.dmg`, and it bundles `habakkuk`. For Linux it's a `.tar.gz`. For Windows it's an `.exe`. The builds aren't signed yet, so read [Unsigned builds](#unsigned-builds) before your first launch.

> [!TIP]
> When you quit the app, it stops a `hk serve` it started. If your agent has to stay up, run `hk serve` [as a service](../deploy/server.md) and let the app connect to it.

## How it works

```
 Habakkuk Desktop                                        a Habakkuk
 ┌──────────────────────────────────────────┐          ┌──────────────────────┐
 │ main window ─┐                           │          │ hk serve         │
 │ quick ask ───┼─> http://127.0.0.1:PORT ──┼── proxy ─┼─> /app/  /api/       │
 │              │   /desktop/  app pages    │  +token  │                      │
 │ tray, hotkey,│   /desktop/api/  app API  │          │                      │
 │ notifications│                           │          └──────────────────────┘
 │ supervisor ──┼── starts and stops ───────┼────────> hk serve (local)
 └──────────────────────────────────────────┘
```

- **One UI codebase.** The main window shows the dashboard of the Habakkuk you're connected to, so you always see the version that Habakkuk ships. The app's windows load everything from a small web server on `127.0.0.1`. That server passes `/app/` and `/api/` on to the active Habakkuk and adds its token. The token never reaches the page. The page also sees one origin, whichever Habakkuk is connected. Because the page loads over loopback (an address only this computer can reach), the microphone works even when the Habakkuk itself is plain `http`.
- **Only the app can use that server.** The server answers only requests addressed to its own loopback address, which stops DNS rebinding. It also needs a session cookie. The app's windows get that cookie by opening a URL with a random key, and the key is new on each launch. The cookie is `HttpOnly` and `SameSite=Strict`. Changes must be JSON, and the server refuses any request a browser marks as cross-site. Another program on your computer can still open a connection to the port, but without the key it gets `403`.
- **The app's own pages** are connections, logs, settings and quick ask. They're static files embedded in the app. Their CSP is the dashboard's strict policy, which allows only the app's own scripts and no inline code.
- **Notifications.** The app follows the event stream (`/api/chat/{conversation}/events`) of each web conversation that was active in the last day, up to eight at once. It notifies you of assistant replies that are newer than the app's start. It asks `/api/approvals` every 5 seconds and the overview every 15 seconds. While Habakkuk isn't answering, it asks every second. An approval prompt is also a chat message, but you're notified of it only once, as an approval with buttons. You don't get a reply notification while the main window has focus, or while the quick ask window is showing that conversation.

## Connections

Each connection is one of two kinds:

- **Local.** This is a Habakkuk on this computer. You point the app at its `HABAKKUK_HOME` (the default is `$HABAKKUK_HOME` or `~/.habakkuk`). The app takes the address and token from that home, the same way `hk serve` reads them. They come from `http.listen` and `http.token_env` in `config.yaml`, with the token itself read from the environment or `.env`. When you activate the connection, the app starts `hk serve` as a child process. The process gets that `HABAKKUK_HOME` and a `PATH` that includes the usual install folders. Its output shows in the connections page. If it crashes, the app restarts it after 1 second, and the wait doubles up to 30. When you pause the connection or quit, the app stops the process with SIGTERM, followed by a kill after 10 seconds. If `serve.json` shows that a `hk serve` is already running there, the app connects to it and leaves it alone. If you never want the app to start it automatically, tick "only when I press Start".
- **Remote.** This is any Habakkuk you can reach over HTTP(S). You give the app a base URL and the token. The URL looks like `https://habakkuk.example.com`, and it can carry a path prefix when the Habakkuk is behind a reverse proxy. If you paste a URL with `/app/` on it, the app drops that part. The app stores the token in the OS keychain. That's the login Keychain on macOS, the Secret Service (GNOME Keyring, KWallet) on Linux, and the Credential Manager on Windows. The token is filed under the service "Habakkuk Desktop", and it's never written to the config file. Before you save, use **Test** to check the URL and token against `/api/app`.

The app keeps the list, the active connection and the settings in `config.json`, with mode 0600. The file is in the OS's per-user config folder: `~/Library/Application Support/Habakkuk Desktop/` on macOS, `%AppData%\Habakkuk Desktop\` on Windows, and `~/.config/Habakkuk Desktop/` on Linux. To keep it somewhere else, set `HABAKKUK_DESKTOP_CONFIG`. The app's log is `desktop.log`, in the same folder.

For a local connection, the app has to find the habakkuk executable. It uses the one set on the connection if there is one. Failing that, it uses the one bundled with the app (the macOS and Linux packages include it). After that it tries `habakkuk` on your `PATH`, and last `~/.local/bin/habakkuk`, `/usr/local/bin/habakkuk` or `/opt/homebrew/bin/habakkuk`.

A local Habakkuk needs an `http` section in its `config.yaml`. If there isn't one, `hk serve` has no dashboard, and the connections page tells you so. The section looks like this:

```yaml
http:
  listen: 127.0.0.1:8484
  token_env: HABAKKUK_HTTP_TOKEN   # optional on loopback; put the value in .env
```

## What you see

This section describes each screen in words, because the repository has no screenshots.

- **Main window.** It shows the Habakkuk dashboard (overview, chat, sessions, board, schedules, memory, skills, usage, approvals, config), the same as in a browser. While a connection is coming up, you see the connections page with a "Connecting" status. The window switches to the dashboard once Habakkuk answers. When you close the window, the app stays in the tray. On macOS the menu bar has File → New Chat (Cmd+N), Quick Ask, Connections… (Cmd+,), and the usual Edit, View and Window menus.
- **Connections page.** Open it with `Connections…`, or from the tray. It starts with a card for the active connection. The card has a coloured dot: green when connected, amber when a channel is down, red when not connected. It also shows the connection's channel and approval counts, and any problem in a red box ("no Habakkuk is set up in ~/.habakkuk yet: run `hk setup`", "config.yaml has no http section", "The token is missing or wrong."). For a local connection, a panel shows `hk serve`. You see its state (running, backoff, stopped, or "running outside the app"), pid, address, restart count and last exit. The panel also has Start and Stop buttons and a scrolling log. Below that is the list of connections. Each one has a radio button to switch to it, a badge (local, remote, or "remote, no token"), and Edit and Remove links. The add/edit form shows URL and Token fields for a remote connection. For a local one it shows HABAKKUK_HOME, executable and "only when I press Start". The settings come last: the hotkey, start at login, and notifications on or off.
- **Tray / menu bar.** On macOS the icon is the Habakkuk mark (two tablets forming an H, with the light above). It's a template image, which macOS tints to match light and dark menu bars. A variant with a dot in the corner shows while approvals are waiting, the connection has a problem, or Habakkuk isn't answering. The number of waiting approvals shows next to it. On Windows and Linux the tray shows the app icon. The menu starts with the status ("Connected to Home"), "Channels: 3 of 3 connected" and "Approvals waiting: 2", which opens the approvals page. Then come Open Habakkuk, New Chat and Quick Ask (with its hotkey). A Connections submenu has one radio item per connection, plus "Manage Connections…". For a local connection there's "Pause hk serve" or "Resume hk serve". The menu ends with a "Start at Login" checkbox and Quit Habakkuk.
- **Quick ask.** Press Cmd+Shift+Space on macOS, or Ctrl+Shift+Space elsewhere. A small window opens in the middle of the screen and floats on top. It has a text box and Send. The reply streams in under your question as the model writes it, and each tool call adds a grey "using exec…" line. When the final message arrives, it replaces the streamed text. The footer says which agent you're asking. It also has New (a fresh conversation) and Open in Habakkuk (the same conversation in the main window's chat). To hide the window, press Escape or click somewhere else. Each quick ask conversation is an ordinary web conversation (`quick-...`), so you'll also find it in the dashboard's chat list and in sessions.
- **Notifications.** An approval shows as "Approval needed: exec" with the tool's summary and **Approve** and **Deny** buttons. Click the notification itself to open the approvals page. A reply shows as "Habakkuk replied" (or the agent's name) with the first 240 characters. Click it to open that conversation.

## Building

To build the app yourself, you need Go (the version in `apps/desktop/go.mod`) with CGO, and your platform's webview:

- **macOS** 11 or later: you need Xcode or the Command Line Tools.
- **Linux**: you need GTK 4 and WebKitGTK 6. On Ubuntu 24.04, for example, run `sudo apt install libgtk-4-dev libwebkitgtk-6.0-dev`.
- **Windows** 10 or 11: you need the WebView2 runtime, which is preinstalled on Windows 11. You don't need a C compiler.

```bash
cd apps/desktop
go test ./...
go build -o habakkuk-desktop .          # a plain binary; run it from a terminal
HABAKKUK_DESKTOP_DEBUG=1 ./habakkuk-desktop   # also logs every request to the app's local server
```

To make the packages the way the release workflow does, run the packaging script:

```bash
VERSION=v0.3.0 apps/desktop/scripts/package.sh darwin    # dist/Habakkuk.app (universal, bundles habakkuk), .zip and .dmg
VERSION=v0.3.0 apps/desktop/scripts/package.sh linux     # dist/Habakkuk-Desktop_v0.3.0_linux_amd64.tar.gz
VERSION=v0.3.0 apps/desktop/scripts/package.sh windows   # dist/Habakkuk-Desktop_v0.3.0_windows_amd64.exe (Git Bash)
```

A Wails v3 app is an ordinary Go program, so the script calls `go build` directly. On macOS it also uses `lipo`, `iconutil`, `codesign` and `hdiutil`. It doesn't use the `wails3` Taskfile scaffolding.

The icons come from the Habakkuk app icon, `assets/brand/app-icon-1024.png`. That file is embedded in `internal/icon` with the menu bar template images. `go run ./cmd/icons` writes the files each package needs. For macOS that's the `.icns`, made through `iconutil`. For Windows it's the `.ico`, which is linked into the executable together with the application manifest (`packaging/windows/habakkuk-desktop.exe.manifest`) and version info. For Linux it's the PNG. `packaging/` also holds the macOS `Info.plist` and the Linux `.desktop` file. The macOS bundle ID is `ai.habakkuk.desktop`.

To install on Linux, unpack the archive. Put `habakkuk-desktop` and `habakkuk` on your `PATH`. Then copy `habakkuk-desktop.desktop` to `~/.local/share/applications/` and `habakkuk-desktop.png` to `~/.local/share/icons/`.

The release workflow builds all three packages on tag pushes. It's the `desktop` job in `.github/workflows/release.yml`. The job runs after the `binaries` job has created the release, and attaches the files to that release.

## Unsigned builds

The release builds aren't signed with a developer certificate, and they aren't notarized. Here's what that means on each OS:

- **macOS** builds have an ad-hoc signature only. The first time, open the app with right-click → Open, or run `xattr -dr com.apple.quarantine /Applications/Habakkuk.app`. macOS asks once whether Habakkuk may send notifications. It asks again after you upgrade from a build with an older bundle ID, because it treats `ai.habakkuk.desktop` as a new app. Start at login uses `SMAppService` on macOS 13 and later, and System Settings → General → Login Items shows it. On earlier versions it uses a LaunchAgent.
- **Windows** SmartScreen warns you about an unknown publisher. Choose More info → Run anyway.
- **Linux** has nothing to sign.

## Limits

- **Notifications cover the web chat only.** You don't get a notification for a reply on Telegram, Slack or the other channels, because you already get the reply there. You do get one for an approval, wherever it's waiting. The app follows only the eight most recently active web conversations of the last day.
- **Notification buttons.** Approve and Deny appear on macOS (in the bundled app) and on Windows. On Linux they appear where the notification server supports actions. A plain `go build` binary on macOS isn't a bundle, so macOS refuses it native notifications. The app then falls back to `osascript` banners, which have no buttons. On Linux the fallback is `notify-send`.
- **Global hotkey.** On Linux the hotkey needs X11, or a Wayland compositor with the GlobalShortcuts portal. Elsewhere it may not register. If the system refuses a hotkey, the settings page tells you and keeps the old one.
- **Windows** packages don't bundle `habakkuk`. Install it with the [Windows installer](../get-started/installation.md#install-script), then point a local connection at that `habakkuk.exe`. Or use remote connections. On Windows, stopping a local serve kills it outright, without asking it to stop first.
- **"New Chat"** opens the dashboard's chat and presses its New chat button for you, because the dashboard has no address for a new chat. **"Open in Habakkuk"** from quick ask opens `#/chat/<conversation>`. The dashboard selects that conversation once it has merged the server's conversation list.
- **Quitting the app stops the `hk serve` it started.** If your agent has to stay up (for channels or schedules), run `hk serve` as a service and let the app connect to it. The app never stops a serve it didn't start.
- The dashboard keeps its own settings (theme, chat list) per origin, so the app uses the same loopback port on every launch. If that port is taken, the app picks another one and those settings start afresh.
- Some things aren't tested in this repository: the Windows and Linux builds (CI builds them), notification buttons, the global hotkey, and start at login on each OS. The tests do cover the connection manager, the event stream client and notifier, the loopback server and proxy, and the supervisor with a fake `habakkuk` binary.
