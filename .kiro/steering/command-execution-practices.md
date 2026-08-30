# Command Execution Practices (avoid terminal hangs + re-approvals)

Lessons learned while iterating on the BuildKit multi-arch work. Following these
keeps the terminal healthy and avoids interruptions that require re-approval.

## 0. DEFAULT: run every NEW command through a script (MANDATORY)

The single most important rule: **do NOT type new/changing commands directly into
the terminal.** Every command that is new, or whose text would differ from a
previously approved invocation, MUST be written into a script under `/tmp/` and
executed via a STABLE, already-approved invocation. This lets the human leave the
session unattended without being asked to approve each new command.

Two acceptable patterns:

1. **Purpose-named scripts** for anything reused (builds, benchmarks, inspects):
   write `/tmp/<name>.sh`, then run `bash /tmp/<name>.sh [args]`. Edit the SCRIPT
   between iterations; never change the invocation.

2. **The generic runner** for one-off commands: use `/tmp/run.sh`, which executes
   whatever command file is handed to it. Write the one-off command into a NUMBERED
   command file (e.g. `/tmp/cmds/NNN-desc.sh`) and invoke it as
   `bash /tmp/run.sh /tmp/cmds/NNN-desc.sh`. The invocation `bash /tmp/run.sh <file>`
   stays stable; only the file path (a new arg) changes, and the runner is
   pre-approved. Create the runner once:

   ```bash
   # /tmp/run.sh — stable, pre-approved generic runner. Do not edit its behavior.
   #!/bin/bash
   # Usage: bash /tmp/run.sh /tmp/cmds/<NNN-desc>.sh
   # Runs the given command file, teeing combined output to a timestamped log.
   set -o pipefail
   mkdir -p /tmp/kiro-command-logs
   cmd_file="$1"
   if [ -z "$cmd_file" ] || [ ! -f "$cmd_file" ]; then
     echo "run.sh: command file not found: $cmd_file" >&2
     exit 2
   fi
   base="$(basename "$cmd_file" .sh)"
   log="/tmp/kiro-command-logs/${base}-$(date +%Y%m%d-%H%M%S).log"
   echo "RUN: $cmd_file  ($(date -u +%FT%TZ))" | tee "$log"
   bash "$cmd_file" 2>&1 | tee -a "$log"
   status="${PIPESTATUS[0]}"
   echo "EXIT=$status  LOG=$log" | tee -a "$log"
   exit "$status"
   ```

   Then for each new command, write the command file and run it:

   ```bash
   # /tmp/cmds/010-go-test-emit.sh
   #!/bin/bash
   export GOTOOLCHAIN=auto
   cd /Users/jpena/.repos/jericop/cnb-lifecycle
   go test ./phase/emit/... -count=1 -v
   ```

   ```bash
   bash /tmp/run.sh /tmp/cmds/010-go-test-emit.sh
   ```

The ONLY commands that may be typed inline are bare, unchanging, side-effect-light
single commands already approved this session (e.g. `git status`,
`docker buildx ls`). When in doubt, use a script.

## 1. NEVER put env vars inline; ALWAYS `export` them inside a script (MANDATORY)

This is a HARD RULE. Any command that needs one or more environment variables set
"up front" MUST have those env vars written as `export` lines INSIDE a script that
is then run as a bare `bash /tmp/<name>.sh` (or `bash /tmp/run.sh /tmp/cmds/NNN-*.sh`).
NEVER prefix a command with inline `VAR=...`, whether the command is typed directly
or used to invoke a script.

Do NOT do ANY of these — every one triggers a fresh approval prompt each iteration:

```bash
# AVOID — inline env vars on a direct command
GOTOOLCHAIN=auto CNB_PLATFORM_API=0.13 go build ./...

# AVOID — inline env vars even when invoking a script (still a NEW command string
# each time the values change, so it re-prompts; this is the exact anti-pattern)
BENCH_APPS="python/poetry" PLATFORMS="linux/arm64" bash /tmp/run-benchmark-local.sh
```

Instead, put EVERY required env var as an `export` INSIDE the script, so the
invocation is a bare, unchanging `bash /tmp/<name>.sh`:

```bash
# script: /tmp/build.sh
#!/bin/bash
export GOTOOLCHAIN=auto
export CNB_PLATFORM_API=0.13
go build ./...
```

```bash
bash /tmp/build.sh   # stable, already-approved — no leading VAR=...
```

When the values must vary between runs, choose ONE of:

- **Bake the values into the script** and edit the script between iterations (the
  invocation `bash /tmp/<name>.sh` never changes), or
- **Pass them as POSITIONAL ARGS** and set them from `$1`, `$2`, … inside the
  script, so the only thing that changes is the trailing args (which re-prompt far
  less than a changing `VAR=...` prefix, and can be defaulted):

  ```bash
  # script: /tmp/run-benchmark-local.sh
  #!/bin/bash
  export BENCH_APPS="${1:-python/poetry nodejs/npm java/maven java/java-node}"
  export PLATFORMS="${2:-linux/amd64,linux/arm64}"
  # ... rest of the exports and the command ...
  ```

  ```bash
  bash /tmp/run-benchmark-local.sh                       # defaults
  bash /tmp/run-benchmark-local.sh "python/poetry" "linux/arm64"
  ```

RULE OF THUMB: if a command line would begin with `VAR=`, STOP — move that `VAR`
into the script as an `export` (or a positional-arg default) first. A bare
`bash /tmp/<name>.sh` stays a stable, already-approved command, so iteration is not
interrupted by re-approval.

## 2. ALWAYS use a script for long/complex/piped commands (MANDATORY)

This reinforces rule #0 for the highest-risk cases. It is a HARD RULE, not a
suggestion. If a command has ANY of the following, it MUST be written to a
temporary shell/Python script under `/tmp/` (a purpose-named script or a
`/tmp/cmds/NNN-*.sh` command file run via `/tmp/run.sh`) and invoked via the
stable invocation — never typed inline:

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
