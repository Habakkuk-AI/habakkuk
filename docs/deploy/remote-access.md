---
title: Remote access
description: "Reach the dashboard and receive webhooks from outside by keeping the listener on loopback, setting a token, and adding a reverse proxy, tunnel or Tailscale."
order: 2
---

Sometimes Habakkuk's HTTP server has to be reachable from outside the machine it runs on. You need that for webhook channels (SMS, Teams, Google Chat and the others) and for a dashboard you open from another device. Here's the safe way to set it up:

1. Keep `listen` on loopback, the address that only programs on the same machine can reach.
2. Set `token_env`, so the API asks for a token.
3. Put a reverse proxy or a tunnel that serves HTTPS in front of it.
4. Set `public_url` to the outside address. Webhook channels use it to know their URLs.

```yaml
http:
  listen: 127.0.0.1:8484
  token_env: HABAKKUK_HTTP_TOKEN
  public_url: https://habakkuk.example.com
```

Run `habakkuk doctor` to see each webhook channel's URL under `public_url`.

## On a tailnet

```bash
habakkuk expose tailscale                # share the HTTP listener on your tailnet (never public)
habakkuk expose tailscale -port 443 -print   # pick the port; -print shows the command without running it
habakkuk expose status
habakkuk expose off
```

A tailnet is your private Tailscale network. `habakkuk expose tailscale` runs `tailscale serve`, which shares the listener over HTTPS with the devices on your tailnet and nobody else. Habakkuk never runs `tailscale funnel`. The command refuses to run unless `http.token_env` is set.

> [!NOTE]
> Twilio, Meta and Microsoft can't reach a tailnet address. For webhook channels, use a public reverse proxy or tunnel.

## Behind a reverse proxy

You can use any proxy that terminates HTTPS and forwards to `listen`. Here's an example with Caddy:

```text
habakkuk.example.com {
    reverse_proxy 127.0.0.1:8484
}
```

The dashboard's Live voice mode uses the microphone, and for that it needs HTTPS (or `localhost`).

> [!WARNING]
> Never publish the listener without `token_env`. Habakkuk refuses to listen on a non-loopback address unless one is set, but a proxy in front of a loopback listener gets around that check.

## From your laptop's terminal

`hk` on your laptop can work on the server's Habakkuk. Run `hk remote add srv ssh://you@server` once. After that, run `hk -r srv chat` or any other command. It goes over ssh, so the HTTP listener can stay on loopback. See [Remote CLI](../interfaces/remote-cli.md).

## Remote desktop app

[Habakkuk Desktop](../interfaces/desktop-app.md#connections) can connect to a remote Habakkuk. Give it the base URL and the token. It keeps the token in the OS keychain.
