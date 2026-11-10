---
title: Media and web search
nav_title: Media and search
description: "Read images and documents, generate images and video, and search the web with Anthropic's hosted search or Brave, Tavily, Exa, SearXNG or DuckDuckGo."
order: 6
---

An agent can work with media and the web. It can read images and documents, generate images and video, search the web, fetch pages and speak. Some of these need a short section in your config, shown below.

## Reading images and documents

Two tools in every agent's `core` toolset do the reading:

- `view_image` shows the model an image file from the workspace (or a URL).
- `read_document` returns the text of a PDF, Word (`.docx`), Excel (`.xlsx`, as tab-separated rows per sheet), PowerPoint (`.pptx`, per slide) or HTML file.

When someone sends an image or a PDF in a chat, it also goes straight to the model. Habakkuk treats what's in a document as data, so it's never followed as instructions.

## Generating images

Add an `images:` section and agents get `image_generate`:

```yaml
images:
  provider: openai          # openai | gemini | fal
  key_env: OPENAI_API_KEY
  model: gpt-image-2        # gemini: gemini-2.5-flash-image; fal: fal-ai/flux/dev
  size: 1024x1024           # the default size
```

There are two more settings. `base_url` overrides the endpoint. `edit_model` (fal only) is the model used when there's an image to edit. Generated files are saved in the workspace, and the agent can send them with `send_file`.

## Generating video

Add a `video:` section and agents get `video_generate`:

```yaml
video:
  provider: openai          # openai | gemini | fal
  key_env: OPENAI_API_KEY
  model: sora-2             # gemini: veo-3.0-generate-001; fal: fal-ai/veo3/fast
  size: 1280x720            # openai: WIDTHxHEIGHT; gemini and fal: an aspect ratio (16:9)
  seconds: 8                # the default length
  timeout: 10m              # how long to wait for a video
```

`image_model` (fal only) is the model used when there's a starting image.

## Web search

The way an agent searches the web depends on its main model:

- **On an `anthropic` provider**, the agent uses Anthropic's hosted web search and fetch tools. You don't need a `search:` section.
- **On any other provider**, the agent gets the `web_search` tool. That tool exists only after a `search:` section names a provider:

```yaml
search:
  provider: brave           # brave | tavily | exa | searxng | duckduckgo
  key_env: BRAVE_API_KEY    # brave, tavily and exa need a key
```

| `provider` | Notes |
|---|---|
| `brave`, `tavily`, `exa` | Need a key in `key_env` |
| `searxng` | Your own instance in `url`; no key |
| `duckduckgo` | No key; scrapes the plain results page, best effort. DuckDuckGo may refuse automated searches |

Set `web: false` on an agent to turn web search off for that agent. Search results are outside content, so they [taint the turn](../deploy/security.md#outside-content-taints-a-turn).

> [!NOTE]
> By default, Habakkuk doesn't contact any search provider. It uses one only after you name it in `search:`.

## Fetching pages

`fetch_url` is in `core`. It fetches a page or file over HTTP(S). It won't fetch loopback, LAN or cloud metadata addresses unless you set `tools.allow_private_urls`. If a page needs JavaScript or a sign-in, use the [browser](browser.md).

## Speaking

Add a `tts:` section and agents get `speak`, a tool that sends a voice message. For more, see [Voice](voice.md) and [Speech](../models/speech.md#text-to-speech).

For every option, see [Architecture: media](../reference/architecture.md#media).
