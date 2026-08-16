Execute shell commands; long-running commands automatically move to background and return a shell ID.

<cross_platform>
Uses mvdan/sh interpreter (Bash-compatible on all platforms including Windows).
Use forward slashes for paths: "ls C:/foo/bar" not "ls C:\foo\bar".
Common shell builtins and core utils available on Windows.
</cross_platform>

<execution_steps>
1. Directory Verification: If creating directories/files, use LS tool to verify parent exists
2. Security Check: Banned commands ({{ .BannedCommands }}) return error - explain to user. Safe read-only commands execute without prompts
3. Command Execution: Execute with proper quoting, capture output
4. Auto-Background: Commands exceeding 1 minute (default, configurable via `auto_background_after`) automatically move to background and return shell ID
5. Output Processing: Truncate if exceeds {{ .MaxOutputLength }} characters
6. Return Result: Include errors, metadata with <cwd></cwd> tags
</execution_steps>

<usage_notes>
- Command required, working_dir optional (defaults to current directory)
- IMPORTANT: Use Grep/Glob/Agent tools instead of 'find'/'grep'. Use View/LS tools instead of 'cat'/'head'/'tail'/'ls'
- Chain with ';' or '&&', avoid newlines except in quoted strings
- Each command runs in independent shell (no state persistence between calls)
- Prefer absolute paths over 'cd' (use 'cd' only if user explicitly requests)
{{- if .RgAvailable }}
- Ripgrep (`rg`) is available; prefer it over `grep` for faster, more intuitive searching
{{- end }}
</usage_notes>

<background_execution>
- Set run_in_background=true to run in a separate background shell; returns a shell ID for managing the process
- Use job_output to view current output, job_kill to terminate a background shell
- IMPORTANT: NEVER use `&` at the end of commands to background them - use run_in_background instead
- Background: long-running servers (`npm start`, `python -m http.server`), watch/monitor tasks (`npm run watch`, `tail -f`), anything expected to run indefinitely
- Not background: build commands, test suites, git operations, file operations, short-lived scripts
</background_execution>

<git_message_quality>
Applies to commit messages, PR titles, and PR bodies:
- Understandable to someone unfamiliar with the codebase: a new contributor reading only the message should understand the problem solved, why it matters, and the impact without opening files or knowing internal code names.
- Avoid code identifiers, filenames, function names, implementation details unless necessary for user-facing impact.
- Bad: "Add NameFromHex with sync.Once lazy init" / Good: "Improve color name lookup performance while keeping startup fast"
</git_message_quality>

<commit_messages>
- Follow <git_message_quality>.
- Concise 1-2 sentences focused on why the change exists and what outcome it enables, not a file list.
- Clear, accurate verbs ("add"=new capability, "update"=enhancement, "fix"=bug fix); avoid generic messages.
- First line under 72 characters; add a body only when needed to explain reasoning/tradeoffs, wrapped at 72 columns.
- Internal-only changes still describe the benefit/maintenance outcome rather than naming private code.
- Bad: "fix: nil pointer in session.go" / Good: "fix: prevent session loading from crashing on missing metadata"
</commit_messages>

<git_commits>
When user asks to create a git commit:

1. Single message with three tool_use blocks (IMPORTANT for speed): `git status`, `git diff` (staged+unstaged), `git log` (style reference).
2. Stage relevant untracked files. Don't commit files already modified at conversation start unless relevant.
3. Analyze staged changes in <commit_analysis> tags: list changed/added files, nature of change (feature/fix/refactor/test/docs), purpose, project impact, check for sensitive info. Don't use tools beyond git context.
4. Draft the message per <commit_messages> and check it against the <git_message_quality> litmus test.
5. Create the commit{{ if eq .Attribution.TrailerStyle "assisted-by" }} with attribution{{ end }} using HEREDOC:
   git commit -m "$(cat <<'EOF'
Commit message here.

{{ if .Attribution.GeneratedWith }}
💘 Generated with Sennit
{{ end}}
{{if eq .Attribution.TrailerStyle "assisted-by" }}

Assisted-by: Sennit:{{ .ModelID }}
{{ end }}
EOF
)"
6. If pre-commit hook fails, retry ONCE; if it fails again, the hook is preventing the commit. If it succeeds but modifies files, you MUST amend.
7. Run git status to verify.

Notes: prefer "git commit -am" when possible, don't stage unrelated files, NEVER update config, don't push, no -i flags, no empty commits, return an empty response, always use -m when rebasing.
</git_commits>

<pull_requests>
{{ if .GhAvailable -}}
Use the `gh` command for ALL GitHub tasks.
{{- end }}

When user asks you to create or update a PR:

1. Single message with multiple tool_use blocks (VERY IMPORTANT for speed): `git status`, `git diff`, check if the branch tracks remote and is up to date, `git log` and `git diff main...HEAD` (full history since main divergence).
2. Create a new branch if needed; commit changes if needed; push with -u if needed.
3. Analyze changes in <pr_analysis> tags: commits since diverging from main, nature of changes, purpose, project impact, sensitive info. Don't use tools beyond git context.
4. Draft the PR message per <git_message_quality>: concise 1-2 bullet summary focused on "why", reflecting ALL changes since main divergence, checked against the litmus test.
5. Create the PR with `gh pr create` using HEREDOC:
   gh pr create --title "title" --body "$(cat <<'EOF'

<summary>

{{ if .Attribution.GeneratedWith -}}
💘 Generated with Sennit
{{- end }}

EOF
)"

Important: return an empty response (user sees gh output), never update git config.
</pull_requests>

<examples>
Good: pytest /foo/bar/tests
Bad: cd /foo/bar && pytest tests
</examples>
