---
title: Reference
description: "Complete lists: every CLI command and chat command, every config.yaml section, the HTTP API, and how Habakkuk is built."
order: 8
---

The reference pages list everything in one place. The guides in the other sections explain when and why to use each piece.

<!-- cards -->
- [CLI](cli.md): every `habakkuk` command and its flags, and every chat command.
- [Configuration](configuration.md): every section of `config.yaml`, and the settings for providers, agents and channels.
- [HTTP API](api.md): the JSON API under `/api/` that the dashboard is built on.
- [Architecture](architecture.md): layers, durable jobs, storage, models, permissions, channels, memory and operations.

The source of truth for configuration is `internal/config`: each setting is documented beside its field.
