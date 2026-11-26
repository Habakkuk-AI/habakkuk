---
title: Dashboard
description: "Turn on the control dashboard at /app/ with an http section, see what its pages do, and find the other endpoints the same server offers."
order: 2
---

With an `http` section in your config, `hk serve` also serves a chat page, a control dashboard, a JSON API and webhooks. You probably have one already. `hk setup` adds an `http` section on loopback (an address only this computer can reach) to every new home. So the dashboard is there from your first `hk serve`, and `serve` prints its address. The section looks like this:

```yaml
http:
  listen: 127.0.0.1:8484            # the default; a public address needs token_env
  token_env: HABAKKUK_HTTP_TOKEN    # Authorization: Bearer <token>
  public_url: https://habakkuk.example.com   # optional: how the internet reaches this server
```

> [!IMPORTANT]
> Without `token_env`, the server accepts only a loopback address. It also answers only requests whose `Host` is a loopback name, so a web page can't reach it through DNS rebinding. To reach the server from anywhere else, set a token and put it behind HTTPS. You can do that with a reverse proxy, or with `hk expose tailscale`. See [Remote access](../deploy/remote-access.md).

## The dashboard

Open `http://127.0.0.1:8484/app/` and you get a control dashboard. It's meant for anyone, not only people who edit config files: everything is in plain words, and the settings people change most are switches and boxes. It opens dark, a grid of glass cards over a night sky; **Theme** at the bottom of the menu switches to light. The dashboard is plain HTML, CSS and JavaScript embedded in the binary. It works over the JSON API under `/api/` and uses the same token (see [HTTP API](../reference/api.md)).

- **Home** shows the time and whether every channel is connected, a box to ask your main agent anything, what needs you (approvals you can answer with **Yes** or **No**), your team and what each agent spent today, your routines with an on/off switch each, today's spend (against the daily limit, when you set one) and recent work.
- **Team** lists every agent: what it does, where it answers, its main and backup models, what it can do (each toolset is a checkbox with a sentence on what it gives), where it posts updates and its spending limits. Change what you need and press **Save**.
- **Who answers where** picks the main agent of each chat channel and gives single chats (a Slack channel, a Telegram group) to another agent. Each chat gets a name for people, kept as a comment in `config.yaml`.
- **Schedules** reads in plain words ("Tuesdays and Thursdays at 10am", "Every 15 minutes"), with what each one does; the exact command and prompt are under **Details**.
- **Skills** has a **Turn off** and **Turn on** button for each skill. It applies to every agent at once, and nothing is deleted.
- **Chat** shows tool calls as they happen. It sends files as attachments and voice recordings for transcription, and it reads replies aloud through `tts`. With `realtime:` it also offers Live mode (see [Voice](../tools/voice.md#live-mode-in-the-dashboard)).
- **Approvals** that are waiting in any chat can be answered from the dashboard. The same page lists each agent's saved approval rules, the calls it makes without asking, and you can remove one. See [Always allow](../agents/approvals.md#always-allow).
- **Status** has channel health, the queue, what is running and recent errors. **Sessions**, **Board**, **Memory**, **Usage** and **Nodes** are there too.
- **Settings** shows your configuration with every secret redacted, the doctor's checks, notifications, and what each chat shows while the agent works and in approval requests.
- **Empty pages explain themselves** and offer a message you can send with one click.
- A service worker lets you install the dashboard as an app, and shows its notifications.

### Saving and applying changes

Every change made in the dashboard is written to `config.yaml` in place: comments, order and every other setting stay as they were, the edited file must load before it's written, and the previous file is kept as `config.yaml.bak`. Spending limits, schedules, skills and chat display settings apply at once. Models, tools, where an agent posts and who answers where apply when `hk serve` restarts, so the dashboard shows a bar: **You saved changes that take effect after a restart**, with **Apply now**. That restarts the server gently, the same as stopping it: turns that were running carry on when it's back, usually in a few seconds, and the page waits for it. A `config.yaml` that doesn't load is refused before anything stops. On Linux and macOS the server keeps its process id, so a service manager (systemd, launchd) sees it keep running.

### Signing in from your phone

Run `hk dashboard link`. It prints the dashboard's address with the token after a `#`, for example `https://agent.example.com/app/#token=...`. Open it once on each of your devices and that browser is signed in; the token is taken out of the address at once, and the part after `#` never reaches a server or its logs. Anyone with the link can use the dashboard, so keep it to your own devices; changing the token cuts every link off. `hk dashboard` alone prints the address. To reach the dashboard from outside your computer, see [Remote access](../deploy/remote-access.md).

### Pages from plugins

A plugin can add a page of its own to the dashboard, listed at the end of the menu. See [Dashboard pages](../tools/plugins.md#dashboard-pages).

## Notifications

The dashboard can send notifications to your phone or computer, even when it's closed. Open **Config**, find **Notifications** and press **Turn on**. Your browser asks for permission once. Do this on each device you want notified. You get a notification when:

- **An approval is waiting.** A tool call needs your yes or no in any chat. The notification says which agent and what kind of thing it wants to do, such as "run a command", never the command itself. Tap it to open **Approvals**.
- **A reply came while no dashboard showed it.** A dashboard chat got a reply and no open dashboard had that chat on screen. Tap to open the chat. An approval request in a chat isn't counted as a reply.
- **A scheduled task failed.** A prompt run failed, or a script failed for the first time in a row. You're not told again until it works.

Each device has a switch for each of the three, under **Notifications**. **Send a test** checks that it works. **Turn off** stops notifications on this device, and **Remove** forgets another device, such as an old phone.

How it works: Habakkuk uses standard Web Push. It sends only to the browsers' own push services: Google's (Chrome and most other Chromium browsers), Mozilla's (Firefox), Apple's (Safari) and Microsoft's (Edge). A device that names any other server is refused, so someone who once got hold of your dashboard token can't keep collecting your notifications on a server of their own. And when the dashboard token changes, every registered device is forgotten, so turn notifications on again on yours. The first time it's needed, it makes a key pair (VAPID) and keeps it in `$HABAKKUK_HOME/push/`, with the devices you registered. Every message is encrypted on your server for the one browser it's for, and goes through that browser's own push service (Google's for Chrome, Mozilla's for Firefox, Apple's for Safari, Microsoft's for Edge). That service can see when a message is sent, but not what it says. No other service or account is involved, and nothing to install.

A few things to know:

- **Https or localhost.** Browsers allow push only on a secure page. On this computer, `http://127.0.0.1` works. From another device, reach the dashboard over `https`, for example with `hk expose tailscale`. See [Remote access](../deploy/remote-access.md).
- **iPhone and iPad.** Add the dashboard to your home screen first (Share, then Add to Home Screen), and turn notifications on from the app you added.
- **What shows on the lock screen.** A notification shows the start of the reply, the agent and kind of action for an approval, or the task's name and error. Your phone's lock screen settings decide whether others can see it.
- **Who you are to the push service.** Each message says how its sender can be reached. Set `http.push_contact` to `mailto:you@example.com` or your own `https` address. The default is your `public_url` when it's `https`.
- **A new machine.** Backups don't include `push/`, since it holds a private key. After you move to a new machine, turn notifications on again on each device.

## Everything the server offers

| Path | What it is | Page |
|---|---|---|
| `/` | The chat page | [Web chat](../channels/web-chat.md) |
| `/app/` | The dashboard | this page |
| `/api/` | The JSON API the dashboard uses | [HTTP API](../reference/api.md) |
| `/healthz` | Channel states and queue depth, without a token | [Observability](../deploy/observability.md) |
| `/hooks/<name>` | Signed webhooks that start a turn | [Integrations: webhooks](../tools/integrations.md#webhooks) |
| `/channels/<name>` | Webhook channels (SMS, Teams, Google Chat and others) | [Webhook channels](../channels/settings.md#webhook-channels) |
| `/v1/` | The OpenAI-compatible API | [Integrations](../tools/integrations.md#openai-compatible-api) |
| `/mcp` | Habakkuk as an MCP server, with `mcp_server.http` | [MCP](../tools/mcp.md#habakkuk-as-an-mcp-server) |
| `/a2a` | Agent2Agent, with `a2a.serve` | [Integrations](../tools/integrations.md#agent2agent) |
| `/nodes` | Device nodes, with a `nodes` section | [Integrations](../tools/integrations.md#device-nodes) |
| `/metrics` | Prometheus metrics | [Observability](../deploy/observability.md#metrics-and-traces) |

## Security

The server is locked down for browsers. It refuses every request a browser sends from another origin. Another origin means another site, another port on localhost, or a sandboxed frame. The rule covers WebSocket handshakes and form posts too. The server never grants CORS, and any change the dashboard makes needs a JSON body. So a web page you visit can't drive the agent, read your chats or pair a device, with or without a token. See [Security](../deploy/security.md#the-local-http-server).
