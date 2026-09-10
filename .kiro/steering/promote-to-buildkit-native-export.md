---
inclusion: manual
---

# Promote code from the history branch to `buildkit-native-export`

How to move CODE-ONLY changes from the development/history branch into the
pristine `buildkit-native-export` branch that the fork's images are published
from. This is the authoritative process; follow it exactly. Promotion is a
DELIBERATE, benchmark-gated step — it is NOT done as part of ordinary feature
work on the history branch.

## The two branches (and their worktrees)

| Role | Branch | Worktree (as of this writing) | Contents |
|---|---|---|---|
| dev / history | `buildkit-native-export-with-history-and-kiro` | `/Users/jpena/.repos/jericop/cnb-pack/buildkit-native-export-with-history-and-kiro` | `main` + full BuildKit dev history + `.kiro/` (specs, steering, settings) + benchmark CI + docs |
| pristine target | `buildkit-native-export` | `/Users/jpena/.repos/jericop/cnb-pack/buildkit-native-export` | code/config only; **zero `.kiro/` files**; upstream-clean |

A dedicated worktree for `buildkit-native-export` ALREADY EXISTS at the path
above (verify with `git worktree list`). Do promotion from THAT worktree. If it
were ever missing, create one with:

```bash
git -C <hist> worktree add ../buildkit-native-export buildkit-native-export
```

`<hist>` in the commands below is the history-branch worktree path
(`/Users/jpena/.repos/jericop/cnb-pack/buildkit-native-export-with-history-and-kiro`);
`<target>` is the pristine worktree
(`/Users/jpena/.repos/jericop/cnb-pack/buildkit-native-export`).

## The rules (authoritative)

### 1. Promote CODE CHANGES ONLY — via `git checkout`, never a merge

The mechanism is a path-scoped `git checkout` of specific CODE files from the
history branch INTO the target worktree — NOT a branch merge. A merge would drag
`.kiro/`, the full dev history, and benchmark-only files across, which is exactly
what the target must never contain. From the target worktree:

```bash
git -C <target> checkout buildkit-native-export-with-history-and-kiro -- <code path> [<code path>...]
```

Then review and commit ON `buildkit-native-export`. Only `git checkout <branch> -- <paths>`
(path-scoped) is allowed; never `git merge`.

### 2. EXCLUDE `.kiro/` entirely — never promoted

`.kiro/` (specs, steering, settings) is dev-only and is NEVER promoted. The
target branch tracks **zero** `.kiro/` files (confirmed: `ls-tree -r --name-only
buildkit-native-export | grep -c '^\.kiro/'` = 0) and MUST stay that way. **This
steering file is itself a `.kiro/` file and is therefore never promoted.** Never
pass a `.kiro/...` path to the promotion `git checkout`.

### 3. EXCLUDE any README/doc that is NOT ALREADY on the target

Existing READMEs on the target MAY be updated; do NOT introduce NEW READMEs (or
other pure-doc/markdown that only exists on the history branch).

READMEs that ALREADY exist on `buildkit-native-export` (these MAY be updated):

- `README.md`
- `pkg/README.md`
- `.github/workflows/actions/release-notes/README.md`
- `.github/workflows/delivery/archlinux/README.md`
- `.github/workflows/delivery/ubuntu/debian/README`
- `pkg/client/testdata/buildpack-multi-platform/README.md`
- `pkg/client/testdata/docker-context/error-cases/config-does-not-exist/README`

README that exists ONLY on the history branch — **do NOT promote it**:

- `internal/build/multiplatform/benchmarks/README.md` (benchmark harness doc)

Apply the same "no new docs" spirit to any other pure-doc/markdown file that
lives only on the history branch: if it is not already tracked on the target, it
does not get promoted.

### 4. Documentation lands in the RFC, not the pack repo

Documentation updates may be STAGED on the history branch, but the place they
ultimately land is the RFC:
`cnb-rfcs .../text/0000-buildkit-multiarch-build.md` — NOT promoted into the pack
repo as new docs. Keep narrative/design/benchmark prose in the RFC; keep the pack
repo's target branch to code + already-present docs only.

## Step-by-step promotion

Run every multi-step / piped command through a command file per the
`command-execution` steering (`bash ~/ai-development/run.sh <file>`); the
snippets below show the logic, not the invocation style.

1. **See the candidate code files** changed since the last promotion, excluding
   `.kiro/`:

   ```bash
   git -C <hist> diff --stat buildkit-native-export..buildkit-native-export-with-history-and-kiro \
     -- . ':(exclude).kiro'
   ```

   From that list, drop any NEW README/doc not already on the target (rule 3 —
   e.g. `internal/build/multiplatform/benchmarks/README.md`) and any
   benchmark-only or history-only file. What remains are the code paths to promote.

2. **Checkout those code paths** into the target worktree (path-scoped, rule 1):

   ```bash
   git -C <target> checkout buildkit-native-export-with-history-and-kiro -- \
     <code path> [<code path>...]
   ```

3. **Build + vet + unit tests on the target** (from `<target>`):

   ```bash
   go build ./...
   go vet ./internal/build/... ./pkg/client/...
   make unit    # or: go test ./...
   ```

4. **Review before commit** — confirm nothing forbidden slipped in:

   ```bash
   git -C <target> status --short
   git -C <target> diff --staged --stat
   git -C <target> ls-tree -r --name-only buildkit-native-export | grep -c '^\.kiro/'   # must stay 0
   ```

   Verify: no `.kiro/` path, no NEW README, no history-only doc, no `/tmp` or
   out-of-repo path, no secrets. Stage explicit paths (`git add <paths>` — never
   `git add .`).

5. **Commit on `buildkit-native-export`** with a clear conventional-commit
   message describing the promoted code change. Do NOT merge; do NOT push as part
   of this checklist unless the release step calls for it.

### Verification checklist (all must hold before the promotion commit)

- [ ] Target branch still tracks **0** `.kiro/` files.
- [ ] **No NEW README/doc** added — only code, or edits to already-present docs.
- [ ] `go build ./...` green on the target.
- [ ] `go vet ./internal/build/... ./pkg/client/...` green on the target.
- [ ] Unit tests (`make unit`) green on the target.
- [ ] `git diff --staged` is 100% code (or already-present-doc) changes, fully
      explainable — no stray files, no `/tmp`/out-of-repo paths, no secrets.

## Downstream: publishing after promotion

Promotion just lands the code on `buildkit-native-export`. Images
(lifecycle → builder → pack) are then published FROM the `buildkit-native-export`
branch/tag per the fork release process — see the `fork-release-process.md`
steering (and the builder/lifecycle publish steering). Do not duplicate that
flow here; reference it.
