---
inclusion: always
---

# Pack Repository Location

When the user says "pack", they mean the **jericop/pack** fork. The fork now uses a
**git worktree layout**: `/Users/jpena/.repos/jericop/cnb-pack` is NOT a working clone
— it is a parent directory containing one subfolder per branch, each a separate
worktree (e.g. `buildkit-native-export/`, `fork-main/`, `main/`,
`buildkit-native-export-with-history-and-kiro/`).

- ALL day-to-day pack development happens in the
  **`buildkit-native-export-with-history-and-kiro`** worktree:
  `/Users/jpena/.repos/jericop/cnb-pack/buildkit-native-export-with-history-and-kiro`
  This worktree keeps the full commit history plus the `.kiro/` specs+steering and the
  fork workflow files. Make code changes here unless the user names a different branch.
- Other worktrees are purpose-specific and generally NOT where dev work goes:
  - `main/` — pristine mirror of upstream `buildpacks/pack` main.
  - `fork-main/` — the fork's default branch (holds fork-only workflows for
    `workflow_dispatch`).
  - `buildkit-native-export/` — the squashed, mergeable, code-only PR branch (no
    `.kiro/`, no fork workflows); do NOT do exploratory dev here.
- Do NOT treat `/Users/jpena/.repos/jericop/cnb-pack` itself as a git repo — run git
  commands inside a specific worktree subfolder.
- The upstream `buildpacks/pack` clone (previously at `/Users/jpena/.repos/buildpacks/pack`)
  has been removed from the workspace and MUST NOT be touched. A local upstream
  reference may exist as a git remote (e.g. `upstream`); use it only as a fetch source.
- The Go module path is `github.com/buildpacks/pack` regardless of worktree, so import
  paths are identical — the distinction is the on-disk worktree, not the module name.
