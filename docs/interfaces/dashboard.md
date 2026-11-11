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

Open `http://127.0.0.1:8484/app/` and you get a control dashboard. Its pages are overview, chat, sessions, board, schedules, memory, skills, usage, approvals and config. The dashboard is plain HTML, CSS and JavaScript embedded in the binary. It works over the JSON API under `/api/` and uses the same token (see [HTTP API](../reference/api.md)).

- **Overview** opens with a **Get started** panel on a new install. The panel has three suggestions: say hi, turn on a morning brief, and teach your agent something. Each one opens Chat with the message ready to send. The panel goes away by itself once the agent has answered a message, or once you have a schedule or a second chat channel. To remove it sooner, use Hide.
- **Empty pages explain themselves.** When Chat, Sessions, Schedules, Memory, Skills or the Board is empty, the page says what belongs there. It also offers a message you can send with one click to fill it.
- **Chat** shows tool calls as they happen. It sends files as attachments and voice recordings for transcription, and it reads replies aloud through `tts`. With `realtime:` it also offers Live mode (see [Voice](../tools/voice.md#live-mode-in-the-dashboard)).
- **Approvals** that are waiting in any chat can be answered from the dashboard. The same page lists each agent's saved approval rules, the calls it makes without asking, and you can remove one. See [Always allow](../agents/approvals.md#always-allow).
- **Notifications** on the Config page turn on Web Push for the device you're on. See [Notifications](#notifications).
- **Config** shows your configuration. The values of every secret variable, secret-named field and URL credential are redacted. For each chat channel, there are two settings you can change on this page. The first is what the chat shows while the agent works: a short status line, every step, or nothing. The second is what approval requests show: a plain summary, or the exact command. When you save, the dashboard edits `config.yaml` in place. The change applies from the next message, and you don't need to restart.
- A service worker lets you install the dashboard as an app, and shows its notifications.

Want the dashboard in a window of its own? [Habakkuk Desktop](desktop-app.md) wraps the same dashboard in a native window and adds notifications.

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
