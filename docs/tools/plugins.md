---
title: Plugins and outside skills
nav_title: Plugins
description: "Install skills and plugins from GitHub, URLs, local paths and registries, with a lock file, a security scan, updates, restores and the plugin manifest."
order: 8
---


Use this page when you want to install skills or plugins that someone else has packaged. A skill is a folder with a `SKILL.md`, in the Agent Skills format. A plugin is a bundle of MCP servers, skills, hooks and chat commands. The page covers the sources Habakkuk installs from, what an install does on disk, the lock file, registries, the security scan and the plugin manifest. If you only want MCP servers, see the [MCP catalog](mcp.md#the-catalog).

```bash
hk skills install github:acme/skills/pdf-forms         # a skill
hk plugins install ./tracker                           # a plugin
hk plugins enable tracker                               # nothing in a plugin loads before this
```

> [!WARNING]
> Skills and plugins run with your agent's permissions. An install prints every file and script and scans them. A clean scan still doesn't make a package safe, so read what you install.

## Sources

You can give `hk skills install` and `hk plugins install` the same kinds of source:

| Source | Example | Fetched as |
|---|---|---|
| GitHub repository or folder | `github:acme/skills`, `github:acme/skills/pdf@v1.2`, `https://github.com/acme/skills/tree/main/pdf` | the repository tarball at a commit, over HTTPS from the GitHub API; no git needed |
| URL to a single file | `https://example.com/pdf/SKILL.md` | that file (a skill's `SKILL.md`, or a plugin's `habakkuk-plugin.yaml`) |
| URL to an archive | `https://example.com/pdf.zip`, `.../pdf.tar.gz` | the archive, unpacked |
| Local path | `./pdf`, `~/src/skills`, `file:/tmp/pdf.zip` | a copy of the folder, file or archive |
| Registry name | `pdf-tools`, `registry:team/pdf-tools` | the source the registry index gives for that name |

A GitHub source can carry a ref: `@branch`, `@tag` or `@commit`. Without one, Habakkuk uses the repository's default branch. It resolves the ref to a commit before the download, and that commit is what the lock file records. A `GITHUB_TOKEN` in the environment or `.env` lifts the anonymous rate limit. URLs have to be `https`. Plain `http` is accepted only for `localhost`, and a redirect to plain `http` is refused.

A source can hold one package or many. Habakkuk tells by the marker file (`SKILL.md` or `habakkuk-plugin.yaml`). If the folder itself holds the marker file, the folder is one package. If it doesn't, every folder below it that holds one is a package. Habakkuk looks at most four levels down and skips hidden folders. So `hk skills install github:acme/skills` installs every skill in the repository. A package takes the `name` it declares, or its folder's name if it declares none. Use `-name` to rename a single package.

## What an install does

An install goes through six steps:

1. **Fetch** the package into a staging folder inside the destination (`$HABAKKUK_HOME/skills/.tmp-*` or `plugins/.tmp-*`). Archives are unpacked with limits: 64 MB downloaded, and 128 MB and 10,000 files unpacked. If an entry would land outside the folder (`..`, absolute paths), the whole install fails. Symlinks, hard links, devices and `.git` are skipped. Every file is written `0600` (folders `0700`), so nothing installed is executable or readable by other users.
2. **Check** that a plugin's manifest parses (see below).
3. **Hash** the package to get its tree hash (see below). For an install from a registry, the tree hash has to match the entry's `sha256`, and the package has to call itself what the entry is called, or nothing is installed.
4. **Scan** every file (see below). Habakkuk prints the source, the pinned commit (or version), the tree hash, every file and the scripts among them, along with the findings. A high-risk finding (`pipe-to-shell`, `obfuscation`, `env-exfiltration`, `credential-files`, `binary`) stops the install unless the command has `-force`. Any other finding stops it unless the command has `-y` (or `-force`).
5. **Move** the package into place with one rename. If a package of the same name is already there, the install is refused unless the command has `-force`. In that case the old package is moved to the backups first.
6. **Record** the package in the lock file, with the commit and the tree hash.

An installed skill starts out **unapproved**. What that means depends on whether it ships scripts. A script is a file with an executable bit, a `#!` line, a script extension such as `.sh`, `.py` or `.js`, or a program binary.

A skill that ships scripts is **disabled** until you approve it, so agents don't see it or load it. A skill without scripts does load, but `skill_view` and `/skill` put a caution in front of it. The caution says the skill comes from a third party. Because of that, the agent must ask before it runs commands, sends data anywhere or changes files outside the workspace because the skill says so. Habakkuk never runs a skill's scripts by itself.

Once you've read the skill, run `hk skills approve NAME`. That enables it and removes the caution.

A plugin starts out **disabled**. Nothing in it loads until you run `hk plugins enable NAME`. That command prints what the plugin brings: the commands its MCP servers and hooks run, its slash commands and the variables it needs. Then it approves the plugin.

## The lock file

The lock file records what's installed and where it came from. Skills have one at `$HABAKKUK_HOME/skills/.lock.json`, and plugins have one at `$HABAKKUK_HOME/plugins/.lock.json`:

```json
{
  "version": 1,
  "packages": {
    "pdf-forms": {
      "source": "github:acme/skills/pdf-forms@main",
      "ref": "main",
      "version": "3f9c1e0d2b7a4c5e8f1a2b3c4d5e6f708192a3b4",
      "sha256": "8c1f...e2",
      "installed_at": "2026-11-07T09:12:00Z",
      "trusted": false,
      "flags": ["scripts/setup.sh:4: pipe-to-shell: curl -fsSL https://... | sh"]
    }
  }
}
```

- `source` is what `update` fetches again. Usually that's what you gave to `install`. If that source held several packages, it's this package's own folder (`github:acme/skills/pdf-forms@main`). `path` is the package's folder inside an archive source.
- `registry` says which registry named the package, and `resolved` is the source that registry pointed at.
- `ref` is the ref you asked for, and `version` is what it resolved to. That's a commit for GitHub, the registry's `version`, or the sha256 of a downloaded file.
- `sha256` is the tree hash of the files as installed.
- `trusted` is the approval. `enabled` (plugins) says whether it loads. `flags` holds the scan findings you accepted with `-y`.

`hk skills update [NAME]` (and `hk plugins update`) fetches each package from its `source` again. A branch moves to its newest commit, and a pinned commit stays where it is. A package that came from a registry is looked up in that registry and in no other, whatever other registries list the same name by then. If that registry no longer lists it, or is no longer in your config, the update fails and the installed version stays.

If a package's files hash the same as before, it's left alone, though its `version` is brought up to date. If they changed, the package is scanned the way an install is. Habakkuk shows you its diff (added, removed and changed files, with a line diff of text files) along with its scripts and findings. The new version replaces the installed one only when you answer yes. `-y` answers for you. Without a terminal and without `-y`, nothing changes.

The old version is moved to the backups. The new one keeps its approval only if it scans clean and changed no script. One that came from a registry also loses its approval when its `SKILL.md` changed (for a plugin, its manifest or a `SKILL.md` inside it), because those files are what you approved: read the new one, then run `hk skills approve NAME` or `hk plugins enable NAME` again.

`hk skills audit` rescans every installed skill and plugin, and every agent's own skills. It lists their scripts and scan findings. It also checks that the installed files still match the hash recorded at install, so a skill edited behind your back shows up. It exits non-zero on a high-risk finding or a changed file.

### Tree hash

The `sha256` of a package is computed over its files. The archive they came in doesn't count, so the same files hash the same from GitHub, a zip or a local folder.

Habakkuk walks the package depth-first, taking each folder's entries in byte order of their names. For every regular file, it hashes these in order: the file's slash path relative to the package folder, a NUL byte, its size in decimal, a NUL byte, then its bytes. Folders, symlinks and `.git` are left out.

`hk skills install` prints the hash. That's the easiest way to get the value for a registry entry.

## Backups and restore

Habakkuk keeps a backup whenever a version goes away. That covers every version an update replaces, every one that `remove` or `-force` takes away, and every skill the curator merges or archives. Each is moved to `$HABAKKUK_HOME/skills/.backups/<name>/<time>/` (or `plugins/.backups/`). It comes with a `backup.json` that says where it came from, why it was backed up, and its lock entry. Habakkuk keeps the newest ten per name.

- `hk skills restore NAME` puts the newest backup back where it came from, along with its lock entry. That means an unapproved skill stays unapproved. Whatever is there now gets backed up first, so you can undo a restore too.
- `hk skills restore -id ID NAME` restores an older backup, and `hk skills restore -list [NAME]` lists the backups.
- `hk plugins restore` works the same way for plugins.

## Registries

A registry is a JSON index served over HTTPS. It lets you install a package by name.

Habakkuk comes with one registry, named `default`: the public directory at [habakkuk.ai/skills](https://www.habakkuk.ai/skills), whose index is `https://habakkuk.ai/skills/index.json`. So `hk skills search QUERY` and `hk skills install NAME` work on a new install. Habakkuk fetches the index only when you run `hk skills` or `hk plugins` with `search`, `install` or `update`, and it sends nothing about you or your setup with the request. A listing in the directory was read by a person, and its entry names the exact commit and the hash of the files that were read, so that is what you get. It is still someone else's work: an install from it is scanned, and starts unapproved, like any other.

You can remove the default, replace it, and add your own in `config.yaml`:

```yaml
skills:
  default_registry: ""        # "" uses the public directory, "off" removes it, a URL replaces it
  registries:                 # yours, searched in order before the default
    - name: team
      url: https://skills.example.com/index.json
```

Your own registries come first. If `team` and the public directory both list `standup`, `hk skills install standup` installs the one from `team`, and the install prints which registry it used. `hk skills install registry:default/standup` asks for the public one.

With `default_registry: "off"` and no `registries`, a bare name is refused and `hk skills search` says there is nothing to search. A build can change the default with `-ldflags "-X github.com/habakkuk-ai/habakkuk/internal/config.DefaultRegistryURL=https://..."`.

The index looks like this:

```json
{
  "version": 1,
  "skills": [
    {
      "name": "pdf-forms",
      "description": "Fill and check PDF forms.",
      "source": "github:acme/skills/pdf-forms@3f9c1e0d2b7a4c5e8f1a2b3c4d5e6f708192a3b4",
      "sha256": "8c1f...e2",
      "version": "1.2.0",
      "tags": ["documents", "pdf"]
    }
  ],
  "plugins": [
    {"name": "tracker", "description": "Issue tracker tools", "source": "https://example.com/tracker.tar.gz", "sha256": "..."}
  ]
}
```

- `name` is what `hk skills install NAME` asks for. Registries are tried in order, and `registry:team/NAME` asks one registry only. The package has to call itself by the same name, or it isn't installed. `-name` lets you choose another.
- `source` is where the files are. It has to be a GitHub repository or folder at a full 40-character commit (`github:owner/repo/path@<commit>`), or an `https` URL to a `SKILL.md` or an archive. A branch, a tag, a local path and another registry name are all refused, because an index is someone else's text: a branch can move, and a path would copy your own files. It has to hold exactly one package.
- `sha256` is required. It's the tree hash the installed files must have, and a mismatch installs nothing. For an archive URL, put a version in the file name, so the address never serves different files.
- A registry served from your own machine (`http://localhost:...`) may point at `http://localhost` sources too. No other registry can.
- `version` is recorded in the lock file. For GitHub sources, the commit is recorded in its place.

`hk skills search QUERY` and `hk plugins search QUERY` list the entries whose name, description or tags contain every word of the query. Entries that match on the name come first.

## The security scan

Before a package is installed, Habakkuk reads every file in it and checks each line for these things:

| Rule | Looks for |
|---|---|
| `pipe-to-shell` | a download piped into a shell or interpreter (`curl ... \| sh`, `wget -O- ... \| python`, `bash <(curl ...)`, `iex (iwr ...)`) |
| `env-exfiltration` | the whole environment (`env`, `printenv`, `/proc/self/environ`, `os.environ`, `process.env`) in a file that also uses the network |
| `credential-files` | SSH keys, cloud and git credentials, the keychain, in a file that also uses the network |
| `write-outside` | writes, copies or deletes under `/`, `~`, `$HOME` or system folders; shell profiles, `crontab`, `authorized_keys`, launch agents, `systemctl enable`; `../../` paths in writes |
| `privilege` | `sudo`, setuid `chmod` |
| `obfuscation` | `base64 -d \| sh`, `eval` of decoded data, `exec(base64.b64decode(...))`, long `\x` escapes, long `String.fromCharCode` lists, runs of 240 or more base64 characters outside images |
| `binary` | an executable program (ELF, Mach-O, PE) |

The scan is a tripwire. It isn't a sandbox. A clean scan doesn't make a package safe, and a finding doesn't make it malicious. Installers often document `curl | sh`, for example. What a finding does mean is this: read that file before you pass `-y` (or `-force` for a high-risk one).

## Plugins

A plugin is a folder with a `habakkuk-plugin.yaml` in it. For example:

```
tracker/
  habakkuk-plugin.yaml
  skills/
    triage/SKILL.md
  hooks/
    log-turn.sh
  bin/
    tracker-mcp
```

### Manifest

```yaml
name: tracker                   # required: lowercase letters, digits, hyphens (at most 64)
description: Issue tracker tools for Habakkuk
version: 1.0.0

# MCP servers, in the format of mcp_servers in config.yaml.
mcp_servers:
  tracker:
    command: ./bin/tracker-mcp  # ./ and ../ are relative to the plugin folder
    args: [--data, "${PLUGIN_DIR}/data"]
    env: {TRACKER_TOKEN: TRACKER_TOKEN}   # variable in the server -> variable Habakkuk takes it from
    risk: network
    approve: [delete_issue]
  search:
    url: https://search.example.com/mcp
    headers: {Authorization: "Bearer ${SEARCH_KEY}"}

# A folder of skill folders (default "skills" when it exists).
skills: skills

# Hooks, as in config.yaml; they run from the plugin folder.
hooks:
  - event: turn_end
    command: ./hooks/log-turn.sh
  - event: tool_before
    match: ^exec$
    command: ./hooks/check-exec.sh
    timeout: 5s

# Chat commands: /<name>.
commands:
  standup:
    description: Write my standup from the tracker
    prompt: |
      Write my standup for {{args}} from the issues assigned to me in the tracker:
      done yesterday, doing today, blocked.
  sync:
    description: Sync the tracker cache
    run: ./bin/sync "$@"
    timeout: 2m
```

An unknown key is an error, the same as in `config.yaml`. Here's what each part does:

- **MCP servers.** Each server becomes an MCP server of the running config, named after the plugin. The server whose key is the plugin's name, or the plugin's only server, is called `<plugin>` (toolset `mcp:<plugin>`). Any other server is called `<plugin>-<key>` (`mcp:tracker-search`). Everything in the [MCP section of the architecture](../reference/architecture.md#mcp) applies. The server gets `PATH`, `HOME` and its `env`. Secrets are given by name, so their values are never stored. `hk doctor` reports missing ones. An agent on the default toolsets (no `toolsets` in `config.yaml`) gets the toolsets of every enabled plugin. An agent that lists its toolsets opts in by listing `mcp:<plugin>`. If a server's name is already in `config.yaml`, the config wins and the plugin's server is left out. `hk plugins list` says so.
- **Skills** in the skills folder are shared with every agent. In precedence, they come below `$HABAKKUK_HOME/skills` and the agent's own skills. Enabling the plugin approves them.
- **Hooks** are added after the hooks in `config.yaml`. They run with `/bin/sh -c` from the plugin folder, because Habakkuk prefixes each one with `cd <plugin folder> &&`.
- **Commands.** There are two kinds. A `prompt` command sends its prompt to the agent as a message of the conversation, so the reply arrives like any other. `{{args}}` is replaced by what follows the command. If the prompt has no `{{args}}`, the arguments are appended. A `run` command runs the shell command in the plugin folder, in the background. It gets the arguments as `$1...` (pass them on with `"$@"`) and in `HABAKKUK_ARGS`. It also gets `HABAKKUK_HOME`, `HABAKKUK_SESSION`, `HABAKKUK_AGENT` and `HABAKKUK_PLUGIN_DIR`. Its output (stdout and stderr, first 16 KB) is the reply. Like an MCP server, it sees `PATH`, `HOME`, `LANG`, `TMPDIR` and `USER`. It doesn't get Habakkuk's environment. At `timeout` (default 60s, at most 10m) it's killed along with its process group. If a command has the same name as one of Habakkuk's own (`/new`, `/retry`, `/skill`, `/model`...), it's left out and a warning goes in the log. Commands work in every chat channel and in `hk chat`.
- `${PLUGIN_DIR}` is replaced by the plugin folder. This works in a server's `command` and `args`, and in hook commands.

### Loading

Enabled plugins are merged into the configuration every time it's loaded. `config.yaml` itself isn't edited. `config.Load` reads `plugins/.lock.json`. For each plugin that's both enabled and approved, it reads the manifest and adds the plugin's servers, hooks, skill folder and commands.

If a plugin's manifest no longer parses, the plugin is left out. So is any part that clashes with `config.yaml`. `hk plugins list` shows what was left out, and Habakkuk still starts.

`hk serve` notices an enable or disable within a few seconds. Hooks change in place. MCP servers, commands and skills need a restart, and the log says so.

### Commands

```
hk plugins                               list installed plugins: enabled or not, source, problems
hk plugins search QUERY                  search the registries
hk plugins install [-y] [-force] [-name N] SOURCE
hk plugins enable NAME                   show what it brings, approve it and turn it on
hk plugins disable NAME
hk plugins update [-y] [-force] [NAME]
hk plugins remove NAME                   moved to the backups
hk plugins restore [-id ID] NAME | restore -list [NAME]
```

## Skills from outside

```
hk skills [-agent A]                     the skills an agent can use, where each comes from, approval
hk skills search QUERY
hk skills install [-y] [-force] [-name N] SOURCE
hk skills approve NAME                   enable a skill with scripts, drop the caution
hk skills update [-y] [-force] [NAME]
hk skills audit                          rescan installed skills and plugins, check their hashes
hk skills remove NAME
hk skills restore [-id ID] NAME | restore -list [NAME]
hk skills curate [-dry-run] [-agent A]
```

Installed skills are shared skills, kept in `$HABAKKUK_HOME/skills/<name>/`. If an agent has its own skill of the same name, that one still wins for that agent.
