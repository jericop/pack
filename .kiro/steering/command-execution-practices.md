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

## 2. Use a temporary script for long or complex commands

If a command is long, has multiple pipes/subshells, heredocs, or multi-step logic,
write it to a temporary shell or Python script (e.g. under `/tmp/`) and execute the
script. Inline megacommands are hard to read, easy to get subtly wrong, and more
likely to hang the terminal. A script is easier to review, re-run, and tweak.

- Shell or Python is fine — use whichever fits the task.
- Keep the log-to-file convention (tee to `/tmp/kiro-command-logs/...`) inside the
  script or when invoking it.

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
