# Command Execution Practices (avoid terminal hangs + re-approvals)

Lessons learned while iterating on the BuildKit multi-arch work. Following these
keeps the terminal healthy and avoids interruptions that require re-approval.

## 1. Put env vars in a script, not inline on the command line

Do NOT prefix commands with inline environment variables, e.g.:

```bash
# AVOID — inline env vars trigger a new-command approval prompt each time
GOTOOLCHAIN=auto CNB_PLATFORM_API=0.13 go build ./...
```

Instead, write the command (with its env vars) into a temporary script and run it
with `bash`:

```bash
# script: /tmp/build.sh
#!/bin/bash
export GOTOOLCHAIN=auto
export CNB_PLATFORM_API=0.13
go build ./...
```

```bash
bash /tmp/build.sh
```

`bash /tmp/<name>.sh` stays a stable, already-approved command, so iteration is not
interrupted by re-approval of a slightly different inline command each time.

## 2. ALWAYS use a script for long/complex/piped commands (MANDATORY)

This is a HARD RULE, not a suggestion. If a command has ANY of the following, it
MUST be written to a temporary shell/Python script under `/tmp/` and invoked as
`bash /tmp/<name>.sh` (optionally with arguments):

- one or more PIPES (`|`), including `... 2>&1 | tee ...` / `| grep` / `| head`,
- subshells or command substitution (`$(...)`, backticks),
- heredocs (`<<'EOF'`),
- inline environment variables (`VAR=... cmd`),
- multiple statements (`;`, `&&`, `||`),
- more than a trivial single command with a couple of flags.

WHY (two reasons, both painful):

1. **Re-approval prompts.** Approvals are keyed on the EXACT command string. Every
   time the inline command text changes even slightly (a different grep pattern, a
   different path, a changed heredoc), it registers as a NEW command and prompts for
   approval again. A stable `bash /tmp/<name>.sh` invocation stays approved across
   iterations — EDIT THE SCRIPT, do not change the invocation.
2. **Terminal hangs.** Complex inline quoting/pipes are the main cause of the
   terminal hanging (see #3).

RULES:

- Put the varying parts (patterns, paths, tags) INSIDE the script or pass them as
  POSITIONAL ARGS, so the invocation `bash /tmp/<name>.sh <args>` stays stable.
- Prefer a few REUSABLE scripts (e.g. `/tmp/bk-inspect.sh`, `/tmp/optA-build.sh`)
  over a fresh inline command each time.
- Shell or Python is fine — use whichever fits.
- Keep the log-to-file convention (tee to `/tmp/kiro-command-logs/...`) INSIDE the
  script.
- Only bare, unchanging single commands (e.g. `docker buildx ls`,
  `git status`) are OK to run inline. When in doubt, use a script.

## 3. Beware improperly terminated / mis-quoted strings (they hang the shell)

An unterminated quote or a shell-special character in a double-quoted string can
leave the terminal waiting for input (a hang), which then blocks further commands.

Common culprits:

- A missing closing quote (`"..."` with no end quote, or an unbalanced `'`).
- A history-expansion `!` in a double-quoted string in `zsh`/`bash` (e.g. `"...!"`)
  — avoid `!` in double-quoted strings entirely.
- `$!` (last background PID) or other `$`-expansions in double-quoted strings that
  expand to something unexpected and break parsing.

Mitigations:

- Prefer single quotes for literal strings that contain `$`, `!`, backticks, etc.
- For anything non-trivial, put it in a script (see #2) where quoting is explicit
  and reviewable, rather than fighting inline shell quoting.
- Avoid `!` on the interactive command line.

## 4. Docker Desktop can stop unexpectedly (auto-update)

If commands suddenly start failing to reach the Docker daemon (socket errors) or
the terminal behaves oddly, check whether Docker Desktop is running — an
auto-update can stop it. Restarting Docker Desktop restores the daemon (and note
that buildx builders/registries are containers that come back up but may need their
docker-network attachments re-verified).
