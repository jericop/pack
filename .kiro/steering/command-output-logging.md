---
inclusion: always
---

# Command Output Logging

When running shell commands whose output I will inspect (tests, builds, gh runs,
docker, etc.), ALWAYS capture the full output to a log file with `tee` BEFORE
filtering. Never rely solely on `tail`/`grep` of live output — that discards the
rest of the log and forces a re-run to see other relevant messages.

## Rules

1. **Tee first, filter second.** Pipe the full combined output (stdout+stderr) to
   a log file with `tee`, then read/grep/tail the FILE.
   ```bash
   <command> 2>&1 | tee ~/tmp/kiro-command-logs/<descriptive-name>.log
   # then inspect:
   grep -iE "FAIL|ERROR" ~/tmp/kiro-command-logs/<descriptive-name>.log
   ```
   Use `tee` (not `>`) so I still see live progress while it is saved.

2. **Unique, descriptive log file per command.** Name the file after what the
   command does, and make it unique so logs are never overwritten. Include a
   timestamp when a command may run more than once.
   ```bash
   log=~/tmp/kiro-command-logs/tier2-ondisk-integration-$(date +%Y%m%d-%H%M%S).log
   <command> 2>&1 | tee "$log"
   ```

3. **Log directory.** Store logs under `~/tmp/kiro-command-logs` (i.e.
   `$HOME/tmp/kiro-command-logs`) so they PERSIST across restarts. Do NOT use
   `/tmp/kiro-command-logs` — `/tmp` is wiped on reboot, and these logs are the
   record we query next session instead of re-running commands or re-hitting MCP
   servers. Create it if it does not exist:
   ```bash
   mkdir -p ~/tmp/kiro-command-logs
   ```

4. **Preserve full logs for review.** Do NOT delete these logs during a task;
   they are the record we review instead of re-running commands. Reference the
   log path when reporting results so it can be reopened.

5. **Applies to long/verbose commands especially** — integration tests, builds,
   `gh run view --log`, docker builds — but is the default for any command whose
   output matters.

## Example

```bash
mkdir -p ~/tmp/kiro-command-logs
log=~/tmp/kiro-command-logs/tier2-ondisk-$(date +%Y%m%d-%H%M%S).log
PACK_TEST_BUILDKIT_ENABLED=1 ... go test ./internal/build/multiplatform/ \
  -run TestOCILayoutOnDiskIntegration -count=1 -timeout=15m -v 2>&1 | tee "$log"
# inspect specific parts without losing the rest:
grep -nE "FAIL|ERROR|panic|--- (FAIL|PASS)" "$log"
```
