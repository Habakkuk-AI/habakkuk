---
title: Media and web search
nav_title: Media and search
description: "Read images and documents, generate images and video, and search the web: hosted search on Anthropic models, or the web_search tool with Brave, Tavily, Exa, SearXNG or DuckDuckGo."
order: 6
---

## Reading images and documents

Every agent's `core` toolset has:

- `view_image`: the model sees an image file from the workspace (or a URL);
- `read_document`: the text of a PDF, Word (`.docx`), Excel (`.xlsx`, as tab-separated rows per sheet), PowerPoint (`.pptx`, per slide) or HTML file.

Images and PDFs that people send in chats also go to the model directly. Document content is treated as data, never as instructions.

## Generating images

With an `images:` section, agents get `image_generate`:

```yaml
images:
  provider: openai          # openai | gemini | fal
  key_env: OPENAI_API_KEY
  model: gpt-image-2        # gemini: gemini-2.5-flash-image; fal: fal-ai/flux/dev
  size: 1024x1024           # the default size
```

`base_url` overrides the endpoint; `edit_model` (fal only) is used when an image to edit is given. Generated files land in the workspace, and the agent can send them with `send_file`.

## Generating video

With a `video:` section, agents get `video_generate`:

```yaml
video:
  provider: openai          # openai | gemini | fal
  key_env: OPENAI_API_KEY
  model: sora-2             # gemini: veo-3.0-generate-001; fal: fal-ai/veo3/fast
  size: 1280x720            # openai: WIDTHxHEIGHT; gemini and fal: an aspect ratio (16:9)
  seconds: 8                # the default length
  timeout: 10m              # how long to wait for a video
```

`image_model` (fal only) is used when a starting image is given.

## Web search

How an agent searches depends on its main model:

- **On an `anthropic` provider**, the agent uses Anthropic's hosted web search and fetch tools. No `search:` section is needed.
- **On any other provider**, the agent gets the `web_search` tool, which exists only once a `search:` section names a provider:

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

`web: false` on an agent turns web search off for it. Search results are outside content: they [taint the turn](../deploy/security.md#outside-content-taints-a-turn).

> [!NOTE]
> Habakkuk contacts no search provider by default. Each one is used only once you name it in `search:`.

## Fetching pages

`fetch_url` (in `core`) fetches a page or file over HTTP(S). It refuses loopback, LAN and cloud metadata addresses unless `tools.allow_private_urls` is set. For pages that need JavaScript or a sign-in, use the [browser](browser.md).

## Speaking

With a `tts:` section, agents get `speak`, which sends a voice message. See [Voice](voice.md) and [Speech](../models/speech.md#text-to-speech).

[Architecture: media](../reference/architecture.md#media) has every option.
