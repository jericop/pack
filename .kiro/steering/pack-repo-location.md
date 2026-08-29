---
inclusion: always
---

# Pack Repository Location

When the user says "pack", they mean the clone of **jericop/pack** that lives in
the `cnb-pack` folder at `/Users/jpena/.repos/jericop/cnb-pack`.

- ALL pack code changes MUST be made in `/Users/jpena/.repos/jericop/cnb-pack`.
- The upstream `buildpacks/pack` clone (previously at `/Users/jpena/.repos/buildpacks/pack`)
  has been removed from the workspace and MUST NOT be touched under any circumstances.
- Do not create, edit, move, or delete files under any `buildpacks/pack` path.
- The Go module path is `github.com/buildpacks/pack` in both repos, so import paths
  are identical — the distinction is the on-disk clone, not the module name.
