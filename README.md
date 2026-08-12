# herdrtabrenamer

A small daemon that names tabs in [herdr](https://herdr.dev) after what is
actually happening inside them: the running program, otherwise the directory.

herdr itself has no name templates. There is only `ui.prompt_new_tab_name`:
`true` asks for a name by hand, `false` generates one — and the generated name
is just the ordinal number. There is no equivalent of tmux's
`automatic-rename` + `automatic-rename-format`, so names are assembled from
outside over the socket API.

```
pi          a panel running pi in ~/pi
nixos       claude in ~/opt/nixos
rmk         an empty shell in ~/projects/rmk
btop        btop, started in that same ~/projects/rmk
```

## How it works

1. Subscribes to server events via `events.subscribe`.
2. Additionally polls the state on a timer — events do not cover everything,
   see below.
3. Re-queries `tab.list` + `pane.list` (plus `pane.process_info` for the lead
   pane), computes the name from the template and calls `tab.rename` wherever
   the two diverged.

Pane state is deliberately not accumulated from events: the payloads are
partial — `pane_agent_detected`, for example, only returns `pane_id` and
`agent`, without `cwd` or the process name. Re-querying is cheaper than
maintaining an incomplete model.

### Why polling and not events alone

herdr does not emit events for part of the changes. Measured on a live session:
a daemon with only a subscription lived for 75 minutes and received **3**
events, while the state changed dozens of times. A direct check: 30 seconds of
a `pane.updated` subscription plus a targeted `pane.agent_status_changed` for a
specific pane produced zero events. Subscribing to the statuses of all panes is
not possible: `pane.agent_status_changed` requires a `pane_id`.

Hence `-poll 3s` (the default). Events give a fast reaction to structural
changes, polling covers everything else. `-poll 0` leaves events only.

## Build and run

```sh
git clone https://github.com/AlexBSoD/herdrtabrenamer
cd herdrtabrenamer
go build -o herdrtabrenamer .

# see what it would do, changing nothing
./herdrtabrenamer -once

# watch and rename
./herdrtabrenamer -apply
```

On Nix:

```sh
nix run github:AlexBSoD/herdrtabrenamer -- -once
```

Without `-apply` the program changes nothing — it only prints the names it
would set.

Stopping it:

```sh
pkill -x herdrtabrenamer
```

Note the `-x`, matching the process name. `pkill -f 'herdrtabrenamer -apply'`
also matches the command line of your own shell and kills it along with the
daemon.

## Flags

| Flag | Default | What it does |
|---|---|---|
| `-format` | `{proc\|dir}` | the name template |
| `-apply` | off | actually rename |
| `-once` | off | one pass and exit |
| `-force` | off | overwrite human-made names too |
| `-poll` | `3s` | state polling interval, `0` means events only |
| `-max-len` | 24 | truncation in characters (0 means unlimited) |
| `-debounce` | `400ms` | coalescing of the event stream |
| `-workspace` | all | restrict to a single `workspace_id` |
| `-state` | `$XDG_STATE_HOME/herdrtabrenamer/labels.json` | file with our own labels, empty means do not remember |
| `-icons` | — | status emoji, if the template uses them |
| `-socket` | `$XDG_CONFIG_HOME/herdr/herdr.sock` | path to the socket |

## Templates

Tokens: `{proc}` `{dir}` `{agent}` `{cwd}` `{title}` `{icon}` `{status}`
`{number}`.

Alternatives separated by `|` take the first non-empty value. Separators
between tokens disappear together with an empty token, so `{agent}:{dir}`
without a detected agent renders `nixos`, not `:nixos`.

```
{proc|dir}                 btop, nixos, rmk               (the default)
{proc|agent}:{dir}         btop:rmk, claude:nixos, rmk
{number}:{dir}             36:nixos
{title}                    Find an alternative to bge-m3 for Open…
{icon} {proc|dir}          🟡 nixos
```

`{title}` is `terminal_title_stripped` from OSC 0/2, which for Claude Code and
pi holds a meaningful line about the current task.

### The name of the running program

`{proc}` is the foreground process of the lead pane, obtained via
`pane.process_info`. The rules:

- shells (`fish`, `bash`, `zsh`, `sh`, …) count as no process at all, otherwise
  every tab would be called `fish`
- for a pane with a detected agent `{proc}` is empty: the agent name is more
  precise, and its foreground group also holds MCP servers
- the name comes from `cmdline`, not from `name` — on NixOS a running `claude`
  shows up as `name=".claude-wrapped"`; the `-wrapped` / `-wrap` wrappers and a
  leading dot are stripped
- from the group we pick the process with `pid == foreground_process_group_id`,
  not the first one in the list

The tab name is taken from the "lead" pane: first a pane with a detected agent,
then the focused one, then the lowest `pane_id`.

## Human-made names are not overwritten

If a tab label looks neither auto-generated (a bare number) nor like our own
work, the tab is marked as named by a human and never touched again. The
`-force` flag disables the protection.

The daemon recognizes "our own work" in two ways:

1. **A state file** — the labels it set are kept in
   `~/.local/state/herdrtabrenamer/labels.json`. The write is atomic (`.tmp` +
   rename); a corrupt or missing file is not an error. Entries of closed tabs
   are pruned during a full pass.
2. **Trying the variants** — it renders the template for other statuses, with
   and without a process, with and without an agent. That covers the cases
   where the state has not been written yet.

Without the state file things broke out of nowhere: a tab was named `btop`, the
program exited, the expected name became `rmk` — and the previous name looked
foreign, because the name of an already finished program cannot be guessed by
enumeration.

When the icon set or the template changes, old names may fail to be recognized
too — then a single run with `-force` is needed:

```sh
./herdrtabrenamer -apply -force -once -format "{proc|agent}:{dir}"
./herdrtabrenamer -apply -format "{proc|agent}:{dir}"
```

## Agent statuses as emoji (optional)

There is nothing to set a tab colour with: the socket API has neither colours
nor styles (`tab.rename` only takes text), and in `config.toml` the only tab bar
option is `ui.tab_bar_position`. The inline styles `{ token, fg, bold, dim }`
exist solely for `ui.sidebar.agents.rows`, that is, in the sidebar and
statically.

The only way to get a coloured dot in the tab bar is an emoji — they carry
their own colour:

```sh
./herdrtabrenamer -apply -format "{icon} {proc|dir}" \
  -icons "blocked=🟥,working=🟨,done=🟩,idle=⬜"
```

The defaults: 🔴 `blocked`, 🟡 `working`, 🟢 `done`, ⚪ `idle`, `unknown` — none.
An empty icon disappears together with its separator.

Do not pick emoji with a variation selector (`⏸️ ▶️ ⚠️ ❗`, containing U+FE0F) —
their width depends on the terminal, and the tab bar jitters on every status
change. The single-width `● ◐ ○ ◆` take one column but carry no colour of their
own.

`-max-len` counts characters, not columns: a double-width emoji at a limit of
24 will actually occupy 25.

## Socket API quirks worth knowing

- **One request per connection.** The server answers and closes the socket right
  away; a second `write` into the same connection yields a `broken pipe`. So
  every call opens its own connection, and the only long-lived one is the
  subscription stream.
- **`params` is mandatory**, even when empty: without it you get
  `invalid_request: missing field 'params'`.
- **Subscriptions are named with dots** (`pane.updated`), while `data.type`
  inside an event uses underscores (`pane_updated`).
- **`pane.agent_status_changed` requires a `pane_id`**, so subscribing to the
  statuses of all panes at once is impossible.
- **Events are far from covering everything** — see the polling section above.
- The full schema: `herdr api schema --json` (protocol 17, ~250 KB, 89 methods).

## Status

Verified against herdr 0.7.5 (protocol 17) and 0.8.0 (protocol 19).

## License

MIT, see [LICENSE](LICENSE).
