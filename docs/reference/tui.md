# Terminal UI

> [!NOTE]
> On macOS the default `ctrl+` shortcuts are `super+` (Command) instead.

`ctrl+g` toggles the help footer, which always shows the bindings that apply
right now.

## Commands

`/` in the editor opens command completion; `ctrl+p` opens the full palette.
Both are fed from the same list, so they never drift apart.

| Command | Key | Does |
|:--|:--|:--|
| `new` | `ctrl+n` | start a new session (aliases: *clear*) |
| `sessions` | `ctrl+s` | switch session |
| `models` | `ctrl+l` | switch model |
| `providers` | | configure providers |
| `doctor` | | check config problems, including live MCP health |
| `stats` | | usage by model and subagent (aliases: *usage*, *tokens*, *cost*) |
| `theme` | | switch colour theme |
| `threads` | `ctrl+e` | the threads dashboard |
| `compact` | | summarize the session |
| `thinking` | | toggle thinking mode |
| `effort` | | reasoning effort |
| `sidebar` | | toggle the sidebar |
| `files` | `ctrl+f` | attach a file |
| `editor` | `ctrl+o` | open `$EDITOR` |
| `todos` | `ctrl+t` | toggle the to-dos / queue pills |
| `notifications` | | notification style |
| `yolo` | `ctrl+y` | skip permission prompts |
| `transparency` | | toggle the background colour |
| `help` | `ctrl+g` | toggle help |
| `init` | | write a project context file |
| `exit` | `ctrl+c` | quit |

Some entries only appear when they apply: `compact`, `files` and `todos` need
an active session; `thinking` and `effort` depend on what the current model
supports; `threads` needs a workspace that supports them; `files` needs a model
that accepts images; `editor` needs `$EDITOR` set; the Docker MCP entries need
Docker's MCP toolkit installed.

Below the built-ins, the same list carries your
[custom commands](../extending/commands.md)
(`user:`/`project:` prefixed), user-invocable
[skills](../extending/skills.md), and prompts from connected
[MCP servers](../extending/mcp.md).

## Keys

### Anywhere

| Key | Action |
|:--|:--|
| `ctrl+p` | command palette |
| `ctrl+g` | toggle help |
| `ctrl+s` | sessions |
| `ctrl+l` (or `ctrl+m`) | models |
| `ctrl+e` | threads |
| `ctrl+y` | toggle yolo |
| `ctrl+z` | suspend |
| `ctrl+c` | quit |

### Editor

| Key | Action |
|:--|:--|
| `enter` | send |
| `shift+enter`, `ctrl+j` | newline |
| `/` | commands |
| `@` | mention a file |
| `ctrl+f` | add an image |
| `ctrl+v` | paste an image from the clipboard |
| `ctrl+o` | open `$EDITOR` |
| `up` / `down` | previous / next prompt from history |
| `ctrl+r` then `{i}` | delete attachment *i* |
| `ctrl+r` then `r` | delete all attachments |
| `esc` | cancel delete mode |

### Chat

| Key | Action |
|:--|:--|
| `esc` `esc` | **interrupt the running turn** |
| `↑` / `↓`, `k` / `j` | scroll |
| `shift+↑` / `shift+↓`, `K` / `J` | one item at a time |
| `b` / `pgup`, `f` / `pgdn` / `space` | page up / down |
| `u` / `d` | half page up / down |
| `g` / `G`, `home` / `end` | top / bottom |
| `space` | expand or collapse the focused block |
| `c` / `y` | copy |
| `esc` | clear selection |
| `shift+←` / `shift+→`, `H` / `L` | scroll horizontally |
| `ctrl+↓` | enter a subagent's session |
| `ctrl+↑` | leave it |
| `ctrl+d` | toggle details |
| `ctrl+t` | toggle to-dos / queue |
| `ctrl+n` | new session |

> [!IMPORTANT]
> Typing a message while the agent is working does not interrupt it — the
> message is steered into the running turn. `esc` `esc` is what stops it. See
> [Steering, tasks and threads](../concepts/delegation.md).

## Rebinding

```bash
option ui keybinding commands super+p
option ui keybinding editor.newline shift+enter super+j
```

Each `option ui keybinding` call **replaces** every shortcut for that action, so
list all the keys you want in one line.

Action names use the groups `editor.*`, `chat.*` and `initialize.*`; global
actions have no prefix. The global ones are `quit`, `help`, `commands`,
`models`, `suspend`, `sessions`, `tab`, `toggle_yolo`, `threads`.

Editor actions: `send_message`, `open_editor`, `newline`, `add_image`,
`paste_image`, `mention_file`, `commands`, `attachment_delete_mode`, `escape`,
`delete_all_attachments`, `history_prev`, `history_next`.

Chat actions: `new_session`, `add_attachment`, `cancel`, `tab`, `details`,
`toggle_pills`, `up`, `down`, `up_down`, `up_one_item`, `down_one_item`,
`up_down_one_item`, `page_up`, `page_down`, `half_page_up`, `half_page_down`,
`home`, `end`, `copy`, `clear_highlight`, `expand`, `scroll_left`,
`scroll_right`, `enter_child_session`, `exit_child_session`,
`prev_child_session`, `next_child_session`.

Initialize actions: `yes`, `no`, `enter`, `switch`.

In JSON, the equivalent is `options.tui.keybindings`, an object whose values are
arrays of keys.

## Appearance

```bash
option ui compact true              # compact chat layout
option ui diff unified              # unified or split diffs
option ui transparent true          # use the terminal background
option ui scrollbar always          # default | always | never
option ui spinner dots              # scramble | pulse | dots | none
option ui completions-max-depth 4
option ui completions-max-items 200
```

`spinner` sets how much the working indicator moves while the agent is
busy:

| Value | What you see |
|---|---|
| `scramble` | The default: a band of glyphs redrawn every frame under a cycling gradient. |
| `pulse` | The same band, but a fixed row of dots with one highlight travelling across it. Periodic instead of random. |
| `dots` | A single braille spinner. |
| `none` | No animated region at all — the label and the elapsed timer only. |

It governs the indicator shown while the model is working, whether that is
thinking or waiting on a tool. A shell command's `Running…` is unaffected:
it has never scrambled, because the scrambled glyphs read as thinking
rather than executing.

A value Sennit does not recognise falls back to `scramble` and is reported
by `sennit doctor` rather than refusing to start.

Themes are switched from the `theme` command rather than config. Moving
through the list previews each palette on the whole screen; `enter` keeps
the highlighted one, `esc` restores the one you started with.

## Notifications

```bash
option notifications auto           # auto | native | osc | bell | disabled
```

`auto` picks native notifications for a local session and OSC sequences over
SSH, detecting OSC 99/777 support automatically. The `notifications` command
changes it interactively.
