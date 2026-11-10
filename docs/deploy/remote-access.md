---
title: Remote access
description: "Reach the dashboard and receive webhook channels from outside: keep the listener on loopback, set a token, and put a reverse proxy, tunnel or Tailscale in front."
order: 2
---

Webhook channels (SMS, Teams, Google Chat and the others) and a dashboard you open from another device need Habakkuk's HTTP server reachable. The safe pattern:

1. Keep `listen` on loopback.
2. Set `token_env`, so the API needs a token.
3. Put a reverse proxy or tunnel with HTTPS in front.
4. Set `public_url` to the outside address, so webhook channels know their URLs.

```yaml
http:
  listen: 127.0.0.1:8484
  token_env: HABAKKUK_HTTP_TOKEN
  public_url: https://habakkuk.example.com
```

`habakkuk doctor` prints each webhook channel's URL under `public_url`.

## On a tailnet

```bash
habakkuk expose tailscale                # share the HTTP listener on your tailnet (never public)
habakkuk expose tailscale -port 443 -print   # pick the port; -print shows the command without running it
habakkuk expose status
habakkuk expose off
```

`habakkuk expose tailscale` runs `tailscale serve`, which shares the listener with the devices on your tailnet only, over HTTPS. Habakkuk never runs `tailscale funnel`. It refuses without `http.token_env`.

> [!NOTE]
> A tailnet address is not reachable by Twilio, Meta or Microsoft. For webhook channels, use a public reverse proxy or tunnel.

## Behind a reverse proxy

Any proxy that terminates HTTPS and forwards to `listen` works. With Caddy, for example:

```text
habakkuk.example.com {
    reverse_proxy 127.0.0.1:8484
}
```

The dashboard's Live voice mode needs HTTPS (or `localhost`) for the microphone.

> [!WARNING]
> Never publish the listener without `token_env`. Habakkuk refuses to listen on a non-loopback address without one, but a proxy in front of a loopback listener bypasses that check.

## From your laptop's terminal

`hk` on your laptop can work on the server's Habakkuk: `hk remote add srv ssh://you@server` once, then `hk -r srv chat` or any other command. It goes over ssh, so the HTTP listener can stay on loopback. See [Remote CLI](../interfaces/remote-cli.md).

## Remote desktop app

[Habakkuk Desktop](../interfaces/desktop-app.md#connections) can connect to a remote Habakkuk: give it the base URL and the token, which it keeps in the OS keychain.
