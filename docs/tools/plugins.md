---
title: Plugins and outside skills
nav_title: Plugins
description: "Install skills and plugins from GitHub, URLs, local paths and registries: the lock file, the security scan, updates and restores, and the plugin manifest."
order: 8
---


Habakkuk can take in work other people have packaged: skills (folders with a `SKILL.md`, the Agent Skills format) and plugins (a bundle of MCP servers, skills, hooks and chat commands). This page describes the sources Habakkuk installs from, what an install does on disk, the lock file, registries, the security scan, and the plugin manifest. For MCP servers on their own, see the [MCP catalog](mcp.md#the-catalog).

```bash
habakkuk skills install github:acme/skills/pdf-forms   # a skill
habakkuk plugins install ./tracker                     # a plugin
habakkuk plugins enable tracker                         # nothing in a plugin loads before this
```

> [!WARNING]
> Skills and plugins run with your agent's permissions. An install prints every file and script and scans them, but a clean scan does not make a package safe. Read what you install.

## Sources

`habakkuk skills install` and `habakkuk plugins install` take the same sources:

| Source | Example | Fetched as |
|---|---|---|
| GitHub repository or folder | `github:acme/skills`, `github:acme/skills/pdf@v1.2`, `https://github.com/acme/skills/tree/main/pdf` | the repository tarball at a commit, over HTTPS from the GitHub API; no git needed |
| URL to a single file | `https://example.com/pdf/SKILL.md` | that file (a skill's `SKILL.md`, or a plugin's `habakkuk-plugin.yaml`) |
| URL to an archive | `https://example.com/pdf.zip`, `.../pdf.tar.gz` | the archive, unpacked |
| Local path | `./pdf`, `~/src/skills`, `file:/tmp/pdf.zip` | a copy of the folder, file or archive |
| Registry name | `pdf-tools`, `registry:team/pdf-tools` | the source the registry index gives for that name |

A GitHub ref (`@branch`, `@tag` or `@commit`) defaults to the repository's default branch. It is resolved to a commit before the download (`GITHUB_TOKEN` in the environment or `.env` lifts the anonymous rate limit), and the commit is what the lock file records. URLs must be `https` (plain `http` is accepted only for `localhost`), and a redirect to plain `http` is refused.

A source can hold one package or many. When the folder itself holds the marker file (`SKILL.md` or `habakkuk-plugin.yaml`), it is one package; otherwise every folder below it that holds one is a package (hidden folders are skipped, at most four levels down), so `habakkuk skills install github:acme/skills` installs every skill in the repository. A package's name is the `name` it declares, else its folder's name; `-name` renames a single package.

## What an install does

1. **Fetch** into a staging folder inside the destination (`$HABAKKUK_HOME/skills/.tmp-*` or `plugins/.tmp-*`). Archives are unpacked with limits (64 MB downloaded, 128 MB and 10,000 files unpacked); an entry that would land outside the folder (`..`, absolute paths) fails the whole install; symlinks, hard links, devices and `.git` are skipped; every file is written `0600` (folders `0700`), so nothing installed is executable or readable by other users.
2. **Check**: a plugin's manifest must parse (below).
3. **Hash**: the package's tree hash (below). A registry entry with a `sha256` must match it, or nothing is installed.
4. **Scan** every file (below). The source, the pinned commit (or version), the tree hash, every file and the scripts among them are printed with the findings. A high-risk finding (`pipe-to-shell`, `obfuscation`, `env-exfiltration`, `credential-files`, `binary`) stops the install unless the command has `-force`; any other finding stops it unless it has `-y` (or `-force`).
5. **Move** the package into place with one rename. A package of the same name already there is refused unless `-force`, and then moved to the backups first.
6. **Record** it in the lock file, with the commit and the tree hash.

Installed skills start **unapproved**. A skill that ships scripts (a file with an executable bit, a `#!` line, a script extension such as `.sh`, `.py` or `.js`, or a program binary) is **disabled** until you approve it: agents neither see nor load it. One without scripts loads, but `skill_view` and `/skill` put a caution in front of it: the skill comes from a third party, so the agent must not run commands, send data anywhere or change files outside the workspace because the skill says so, without asking. Habakkuk never runs a skill's scripts by itself. `habakkuk skills approve NAME` enables it and removes the caution once you have read it.

Plugins start **disabled**: nothing in them loads until `habakkuk plugins enable NAME`, which prints what the plugin brings (the commands its MCP servers and hooks run, its slash commands, the variables it needs) and approves it.

## The lock file

`$HABAKKUK_HOME/skills/.lock.json` and `$HABAKKUK_HOME/plugins/.lock.json`:

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

- `source` is what `update` fetches again: what you gave to `install`, or, for one package of a source that held several, that package's own folder (`github:acme/skills/pdf-forms@main`). `path` is the package's folder inside an archive source.
- `registry` and `resolved` say which registry named it and the source the registry pointed at.
- `ref` is the ref asked for; `version` what it resolved to: a commit for GitHub, the registry's `version`, or the sha256 of a downloaded file.
- `sha256` is the tree hash of the installed files.
- `trusted` is the approval; `enabled` (plugins) whether it loads; `flags` the scan findings you accepted with `-y`.

`habakkuk skills update [NAME]` (and `habakkuk plugins update`) fetches each package from its `source` again, so a branch moves to its newest commit and a pinned commit stays where it is. A package whose files hash the same is left alone (its `version` is brought up to date). A changed one is scanned like an install and its diff is shown (added, removed and changed files, with a line diff of text files), with its scripts and findings; it replaces the installed one only when you answer yes (`-y` answers for you; without a terminal and without `-y` nothing changes). The old version is moved to the backups, and the new one keeps its approval only if it scans clean and changed no script.

`habakkuk skills audit` rescans every installed skill and plugin and every agent's own skills: it lists their scripts and scan findings, and checks that installed files still match the hash recorded at install, so a skill edited behind your back shows up. It exits non-zero on a high-risk finding or a changed file.

### Tree hash

The `sha256` of a package is computed over its files, not over the archive they came in, so the same files hash the same from GitHub, a zip or a local folder: for every regular file, in a depth-first walk that takes each folder's entries in byte order of their names, its slash path relative to the package folder, a NUL byte, its size in decimal, a NUL byte, then its bytes. Folders, symlinks and `.git` are left out. `habakkuk skills install` prints it, which is the easiest way to get the value for a registry entry.

## Backups and restore

Every version that an update replaces, that `remove` or `-force` takes away, and every skill the curator merges or archives is moved to `$HABAKKUK_HOME/skills/.backups/<name>/<time>/` (or `plugins/.backups/`), with a `backup.json` saying where it came from, why, and its lock entry. The newest ten per name are kept.

- `habakkuk skills restore NAME` puts the newest backup back where it came from, with its lock entry (so an unapproved skill stays unapproved). Whatever is there now is backed up first, so a restore can itself be undone.
- `habakkuk skills restore -id ID NAME` restores an older one; `habakkuk skills restore -list [NAME]` lists them.
- `habakkuk plugins restore` does the same for plugins.

## Registries

A registry is a JSON index served over HTTPS. Configure them in `config.yaml`:

```yaml
skills:
  default_registry: ""        # the index named "default"; empty uses the build's default (none), "off" turns it off
  registries:
    - name: team
      url: https://skills.example.com/index.json
```

Habakkuk ships with no default registry. A build can set one with `-ldflags "-X github.com/habakkuk-ai/habakkuk/internal/config.DefaultRegistryURL=https://..."`.

The index:

```json
{
  "version": 1,
  "skills": [
    {
      "name": "pdf-forms",
      "description": "Fill and check PDF forms.",
      "source": "github:acme/skills/pdf-forms@v1.2.0",
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

- `name` is what `habakkuk skills install NAME` asks for (registries are tried in order; `registry:team/NAME` asks one).
- `source` is any non-registry source above. It must hold exactly one package.
- `sha256`, when present, is the tree hash the installed files must have; a mismatch installs nothing. Pin `source` to a commit or a versioned archive when you set it, or the next upstream change breaks the install.
- `version` is recorded in the lock file (GitHub sources record the commit instead).

`habakkuk skills search QUERY` and `habakkuk plugins search QUERY` list the entries whose name, description or tags contain every word of the query, name matches first.

## The security scan

Every file of a package is read before it is installed, and each line is checked for:

| Rule | Looks for |
|---|---|
| `pipe-to-shell` | a download piped into a shell or interpreter (`curl ... \| sh`, `wget -O- ... \| python`, `bash <(curl ...)`, `iex (iwr ...)`) |
| `env-exfiltration` | the whole environment (`env`, `printenv`, `/proc/self/environ`, `os.environ`, `process.env`) in a file that also uses the network |
| `credential-files` | SSH keys, cloud and git credentials, the keychain, in a file that also uses the network |
| `write-outside` | writes, copies or deletes under `/`, `~`, `$HOME` or system folders; shell profiles, `crontab`, `authorized_keys`, launch agents, `systemctl enable`; `../../` paths in writes |
| `privilege` | `sudo`, setuid `chmod` |
| `obfuscation` | `base64 -d \| sh`, `eval` of decoded data, `exec(base64.b64decode(...))`, long `\x` escapes, long `String.fromCharCode` lists, runs of 240 or more base64 characters outside images |
| `binary` | an executable program (ELF, Mach-O, PE) |

It is a tripwire, not a sandbox. A clean scan does not make a package safe and a finding does not make it malicious (installers often document `curl | sh`); a finding means: read that file before you pass `-y` (or `-force` for a high-risk one).

## Plugins

A plugin is a folder with a `habakkuk-plugin.yaml`:

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

Unknown keys are errors, as in `config.yaml`.

- **MCP servers.** Each server becomes an MCP server of the running config named after the plugin: the server whose key is the plugin's name, or the plugin's only server, is `<plugin>` (toolset `mcp:<plugin>`); others are `<plugin>-<key>` (`mcp:tracker-search`). Everything in the [MCP section of the architecture](../reference/architecture.md#mcp) applies: the server gets `PATH`, `HOME` and its `env`, secrets are named, never stored, and `habakkuk doctor` reports missing ones. Agents that use the default toolsets (no `toolsets` in `config.yaml`) get every enabled plugin's toolsets; an agent that lists its toolsets opts in by listing `mcp:<plugin>`. A server whose name is already in `config.yaml` is left out (the config wins) and `habakkuk plugins list` says so.
- **Skills** in the skills folder are shared with every agent, below `$HABAKKUK_HOME/skills` and the agent's own skills in precedence. Enabling the plugin approves them.
- **Hooks** are added after the hooks in `config.yaml` and run with `/bin/sh -c` from the plugin folder (Habakkuk prefixes `cd <plugin folder> &&`).
- **Commands.** A `prompt` command sends its prompt to the agent as a message of the conversation (`{{args}}` is replaced by what follows the command, or the arguments are appended when the prompt has no `{{args}}`), so the reply arrives like any other. A `run` command runs the shell command in the plugin folder with the arguments as `$1...` (pass them on with `"$@"`) and in `HABAKKUK_ARGS`, plus `HABAKKUK_HOME`, `HABAKKUK_SESSION`, `HABAKKUK_AGENT` and `HABAKKUK_PLUGIN_DIR`, in the background; its output (stdout and stderr, first 16 KB) is the reply. Like an MCP server it sees `PATH`, `HOME`, `LANG`, `TMPDIR` and `USER`, not Habakkuk's environment. It is killed with its process group at `timeout` (default 60s, at most 10m). A command whose name is one of Habakkuk's own (`/new`, `/retry`, `/skill`, `/model`...) is left out with a warning in the log. Commands work in every chat channel and in `habakkuk chat`.
- `${PLUGIN_DIR}` in a server's `command` and `args`, and in hook commands, is replaced by the plugin folder.

### Loading

Enabled plugins are merged into the configuration whenever it is loaded, without editing `config.yaml`: `config.Load` reads `plugins/.lock.json`, and for each plugin that is both enabled and approved it reads the manifest and adds its servers, hooks, skill folder and commands. A plugin whose manifest no longer parses, or a part that clashes with `config.yaml`, is left out and listed by `habakkuk plugins list`; it never stops Habakkuk from starting.

`habakkuk serve` notices an enable or disable within a few seconds: hooks change in place, and MCP servers, commands and skills need a restart, which the log says.

### Commands

```
habakkuk plugins                         list installed plugins: enabled or not, source, problems
habakkuk plugins search QUERY            search the registries
habakkuk plugins install [-y] [-force] [-name N] SOURCE
habakkuk plugins enable NAME             show what it brings, approve it and turn it on
habakkuk plugins disable NAME
habakkuk plugins update [-y] [-force] [NAME]
habakkuk plugins remove NAME             moved to the backups
habakkuk plugins restore [-id ID] NAME | restore -list [NAME]
```

## Skills from outside

```
habakkuk skills [-agent A]               the skills an agent can use, where each comes from, approval
habakkuk skills search QUERY
habakkuk skills install [-y] [-force] [-name N] SOURCE
habakkuk skills approve NAME             enable a skill with scripts, drop the caution
habakkuk skills update [-y] [-force] [NAME]
habakkuk skills audit                    rescan installed skills and plugins, check their hashes
habakkuk skills remove NAME
habakkuk skills restore [-id ID] NAME | restore -list [NAME]
habakkuk skills curate [-dry-run] [-agent A]
```

Installed skills are shared skills (`$HABAKKUK_HOME/skills/<name>/`); an agent's own skill of the same name still wins for that agent.
