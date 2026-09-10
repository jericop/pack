package multiplatform

import (
	"fmt"
	"strings"

	"github.com/moby/buildkit/client/llb"
)

// This file implements a RESTRICTED Dockerfile -> LLB translator for CNB image
// extension Dockerfiles (build.Dockerfile / run.Dockerfile), so the buildkit backend
// can apply extensions natively in the LLB graph WITHOUT kaniko. The daemon lifecycle
// uses kaniko because it has no BuildKit; here BuildKit IS the Dockerfile engine, so a
// kaniko-in-LLB step would be redundant.
//
// The CNB image-extension spec (buildpacks/spec/image_extension.md) restricts extension
// Dockerfiles to a fixed instruction set: FROM (exactly one), ADD, ARG, COPY, ENV,
// LABEL, RUN, SHELL, USER, WORKDIR — "SHOULD NOT contain any other instructions". We
// parse exactly that subset and REJECT anything else (a spec violation we surface rather
// than silently execute). See spec buildkit-extension-support.

// extInstrKind is one of the CNB-allowed extension Dockerfile instructions.
type extInstrKind string

const (
	instrFrom    extInstrKind = "FROM"
	instrArg     extInstrKind = "ARG"
	instrEnv     extInstrKind = "ENV"
	instrRun     extInstrKind = "RUN"
	instrCopy    extInstrKind = "COPY"
	instrAdd     extInstrKind = "ADD"
	instrLabel   extInstrKind = "LABEL"
	instrUser    extInstrKind = "USER"
	instrWorkdir extInstrKind = "WORKDIR"
	instrShell   extInstrKind = "SHELL"
)

// allowedExtInstr is the exact set permitted by the CNB image-extension spec. Any verb
// outside this set is rejected by parseExtensionDockerfile.
var allowedExtInstr = map[string]extInstrKind{
	"FROM":    instrFrom,
	"ARG":     instrArg,
	"ENV":     instrEnv,
	"RUN":     instrRun,
	"COPY":    instrCopy,
	"ADD":     instrAdd,
	"LABEL":   instrLabel,
	"USER":    instrUser,
	"WORKDIR": instrWorkdir,
	"SHELL":   instrShell,
}

// extInstr is one parsed instruction: its kind and the raw argument text following the
// verb (already line-continuation-joined). Structured interpretation (splitting COPY
// src/dst, RUN shell vs exec form, LABEL key=value) happens in the translator.
type extInstr struct {
	kind extInstrKind
	args string
}

// extendDockerfile is a parsed, validated CNB extension Dockerfile.
type extendDockerfile struct {
	instructions []extInstr
}

// parseExtensionDockerfile parses a CNB extension Dockerfile into instructions,
// enforcing the CNB-allowed subset. It errors on: an unknown/disallowed verb, a second
// FROM, or an instruction appearing before the first FROM (other than ARG, which the
// Dockerfile grammar allows before FROM — e.g. the mandatory `ARG base_image`).
func parseExtensionDockerfile(src []byte) (*extendDockerfile, error) {
	logical, err := joinContinuations(string(src))
	if err != nil {
		return nil, err
	}
	df := &extendDockerfile{}
	sawFrom := false
	for _, line := range logical {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue // blank or comment (parser directives are not used by extensions)
		}
		verb, rest := splitVerb(trimmed)
		kind, ok := allowedExtInstr[strings.ToUpper(verb)]
		if !ok {
			return nil, fmt.Errorf("extension Dockerfile: instruction %q is not permitted "+
				"(CNB extensions may only use FROM, ADD, ARG, COPY, ENV, LABEL, RUN, SHELL, USER, WORKDIR)", verb)
		}
		if kind == instrFrom {
			if sawFrom {
				return nil, fmt.Errorf("extension Dockerfile: multiple FROM instructions are not permitted (exactly one allowed)")
			}
			sawFrom = true
		} else if !sawFrom && kind != instrArg {
			return nil, fmt.Errorf("extension Dockerfile: instruction %q appears before FROM", verb)
		}
		df.instructions = append(df.instructions, extInstr{kind: kind, args: rest})
	}
	if !sawFrom {
		return nil, fmt.Errorf("extension Dockerfile: no FROM instruction found")
	}
	return df, nil
}

// joinContinuations splits src into logical lines, joining physical lines that end with
// a backslash continuation. Trailing CRs are stripped so CRLF files parse.
func joinContinuations(src string) ([]string, error) {
	var out []string
	var buf strings.Builder
	continuing := false
	for _, raw := range strings.Split(src, "\n") {
		physical := strings.TrimRight(raw, "\r")
		trimmedRight := strings.TrimRight(physical, " \t")
		if strings.HasSuffix(trimmedRight, "\\") {
			buf.WriteString(strings.TrimSuffix(trimmedRight, "\\"))
			buf.WriteString(" ")
			continuing = true
			continue
		}
		buf.WriteString(physical)
		out = append(out, buf.String())
		buf.Reset()
		continuing = false
	}
	if continuing {
		// Dangling continuation at EOF — keep what we have rather than error.
		out = append(out, buf.String())
	}
	return out, nil
}

// splitVerb splits a logical line into its leading verb and the remaining argument text.
func splitVerb(line string) (verb, rest string) {
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimSpace(line[i+1:])
}

// extImageConfig captures the image-config mutations an extension Dockerfile makes
// (ENV, LABEL, USER, WORKDIR) so the caller can apply them to the extended image's
// config after the LLB state is built. RUN/COPY/ADD mutate the filesystem (the llb.State);
// these mutate metadata.
type extImageConfig struct {
	env     map[string]string // ENV key=value (also threaded onto later RUNs)
	labels  map[string]string // LABEL key=value (incl. io.buildpacks.rebasable)
	user    string            // last USER, "" if unset
	workdir string            // last WORKDIR, "" if unset
}

// extendInput is everything the translator needs to turn one extension Dockerfile into
// an extended llb.State for a single platform leg.
type extendInput struct {
	base       llb.State         // the FROM base (build or run image state for this leg)
	contextSrc llb.State         // build context (context.build|context.run|context|app)
	buildArgs  map[string]string // base_image, build_id (UUID), user_id, group_id, + extend-config.toml
	plat       string            // "linux/arm64" progress-name prefix
}

// toLLB translates the parsed extension Dockerfile into an extended llb.State plus the
// image-config mutations. It applies instructions in order, FROM ${base_image} resolving
// to in.base. RUN/COPY/ADD mutate the filesystem; ENV/ARG feed later RUNs; USER/WORKDIR/
// SHELL set the state for subsequent RUNs; LABEL/ENV/USER/WORKDIR are recorded in the
// returned extImageConfig. When a RUN references $build_id, that vertex is marked
// IgnoreCache so it (and everything after) rebuilds each build, per the spec.
func (d *extendDockerfile) toLLB(in extendInput) (llb.State, extImageConfig, error) {
	cfg := extImageConfig{env: map[string]string{}, labels: map[string]string{}}
	// args holds ARG + build-arg values available for interpolation; env holds ENV values
	// (which also become process env on RUN). Start args from the supplied build args.
	args := map[string]string{}
	for k, v := range in.buildArgs {
		args[k] = v
	}
	state := in.base
	shell := []string{"/bin/sh", "-c"} // default shell for shell-form RUN
	var curUser, curDir string

	interp := func(s string) string { return interpolate(s, args, cfg.env) }

	for _, ins := range d.instructions {
		switch ins.kind {
		case instrFrom:
			// FROM ${base_image} — base is already in.base; nothing to switch. (A
			// run.Dockerfile may FROM a different image, but the caller resolves the run
			// image and passes it as in.base, so FROM is a no-op at translate time.)
		case instrArg:
			k, v, hasVal := splitKeyValue(ins.args)
			if hasVal {
				if _, preset := args[k]; !preset {
					args[k] = interp(v) // ARG default, unless a build arg already set it
				}
			} else if _, ok := args[k]; !ok {
				args[k] = ""
			}
		case instrEnv:
			for k, v := range parseKeyValues(ins.args) {
				val := interp(v)
				cfg.env[k] = val
				args[k] = val
			}
		case instrLabel:
			for k, v := range parseKeyValues(ins.args) {
				cfg.labels[k] = interp(v)
			}
		case instrUser:
			curUser = interp(strings.TrimSpace(ins.args))
			cfg.user = curUser
		case instrWorkdir:
			curDir = interp(strings.TrimSpace(ins.args))
			cfg.workdir = curDir
		case instrShell:
			if parsed, ok := parseJSONArray(ins.args); ok {
				shell = parsed
			}
		case instrRun:
			argv, referencesBuildID := runArgv(ins.args, shell, args, cfg.env)
			runOpts := []llb.RunOption{
				llb.Args(argv),
				llb.WithCustomNamef("[%s] extend: RUN %s", in.plat, truncateForName(ins.args)),
			}
			for k, v := range cfg.env {
				runOpts = append(runOpts, llb.AddEnv(k, v))
			}
			if curUser != "" {
				runOpts = append(runOpts, llb.User(curUser))
			}
			if curDir != "" {
				runOpts = append(runOpts, llb.Dir(curDir))
			}
			if referencesBuildID {
				// $build_id changes every build; mark the vertex uncacheable so it and
				// all subsequent layers rebuild, matching the CNB build_id contract.
				runOpts = append(runOpts, llb.IgnoreCache)
			}
			state = state.Run(runOpts...).Root()
		case instrCopy, instrAdd:
			src, dst, err := parseCopyArgs(interp(ins.args))
			if err != nil {
				return state, cfg, err
			}
			copyInfo := &llb.CopyInfo{CreateDestPath: true, AllowWildcard: true, AllowEmptyWildcard: true}
			var action *llb.FileAction
			for i, s := range src {
				c := llb.Copy(in.contextSrc, s, dst, copyInfo)
				if i == 0 {
					action = c
				} else {
					action = action.Copy(in.contextSrc, s, dst, copyInfo)
				}
			}
			if action != nil {
				state = state.File(action, llb.WithCustomNamef("[%s] extend: %s", in.plat, ins.kind))
			}
		}
	}
	return state, cfg, nil
}

// interpolate substitutes $VAR and ${VAR} references using args first, then env. Unknown
// variables expand to empty string (Dockerfile semantics). A doubled $$ is a literal $.
func interpolate(s string, args, env map[string]string) string {
	lookup := func(name string) string {
		if v, ok := args[name]; ok {
			return v
		}
		return env[name]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end >= 0 {
				b.WriteString(lookup(s[i+2 : i+2+end]))
				i = i + 2 + end
				continue
			}
		}
		// bare $NAME: consume [A-Za-z0-9_]
		j := i + 1
		for j < len(s) && (isNameByte(s[j])) {
			j++
		}
		if j > i+1 {
			b.WriteString(lookup(s[i+1 : j]))
			i = j - 1
			continue
		}
		b.WriteByte('$')
	}
	return b.String()
}

func isNameByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// referencesVar reports whether s contains a $name / ${name} reference to the given var.
func referencesVar(s, name string) bool {
	return strings.Contains(s, "$"+name) || strings.Contains(s, "${"+name+"}")
}

// splitKeyValue splits "KEY=value" (or "KEY value" for single-arg ENV/ARG forms). Returns
// hasVal=false when there is no value (e.g. bare `ARG foo`).
func splitKeyValue(s string) (key, val string, hasVal bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '='); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
	}
	return s, "", false
}

// parseKeyValues parses "K1=v1 K2=v2" (and the legacy "KEY value" single-pair form) used
// by ENV and LABEL. Values may be quoted.
func parseKeyValues(s string) map[string]string {
	out := map[string]string{}
	toks := splitRespectingQuotes(s)
	// If there is no '=' at all, treat as the legacy "KEY rest..." single-pair form.
	if len(toks) >= 2 && !strings.Contains(s, "=") {
		out[toks[0]] = unquote(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), toks[0])))
		return out
	}
	for _, t := range toks {
		if i := strings.IndexByte(t, '='); i >= 0 {
			out[t[:i]] = unquote(t[i+1:])
		}
	}
	return out
}

// runArgv builds the argv for a RUN instruction. Exec form (JSON array) is used verbatim;
// shell form is wrapped with the current SHELL. Reports whether the command references
// $build_id (so the caller can bust the cache).
func runArgv(args string, shell []string, argMap, env map[string]string) (argv []string, referencesBuildID bool) {
	referencesBuildID = referencesVar(args, "build_id")
	if parsed, ok := parseJSONArray(args); ok {
		return parsed, referencesBuildID
	}
	cmd := interpolate(args, argMap, env)
	return append(append([]string{}, shell...), cmd), referencesBuildID
}

// parseCopyArgs splits COPY/ADD args into sources + destination. Flags (e.g. --chown,
// --from) are not supported by the CNB subset in a meaningful way for context copies; a
// --from flag is rejected because extensions copy from the build context, not a stage.
func parseCopyArgs(s string) (src []string, dst string, err error) {
	toks := splitRespectingQuotes(s)
	var positional []string
	for _, t := range toks {
		if strings.HasPrefix(t, "--from=") {
			return nil, "", fmt.Errorf("extension Dockerfile: COPY --from is not supported (extensions copy from the build context)")
		}
		if strings.HasPrefix(t, "--") {
			continue // ignore other flags (e.g. --chown) — ownership handled by lifecycle
		}
		positional = append(positional, unquote(t))
	}
	if len(positional) < 2 {
		return nil, "", fmt.Errorf("extension Dockerfile: COPY/ADD requires at least one source and a destination")
	}
	dst = positional[len(positional)-1]
	src = positional[:len(positional)-1]
	return src, dst, nil
}

// parseJSONArray parses a Dockerfile exec-form array like `["a","b"]`. Returns ok=false
// if s is not a JSON array (i.e. it is shell form).
func parseJSONArray(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, false
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []string{}, true
	}
	var out []string
	for _, part := range splitRespectingQuotes(strings.ReplaceAll(inner, ",", " ")) {
		out = append(out, unquote(part))
	}
	return out, true
}

// splitRespectingQuotes splits on whitespace but keeps quoted spans (single or double)
// together.
func splitRespectingQuotes(s string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ' ' || c == '\t':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// unquote strips a single surrounding pair of matching quotes.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// truncateForName shortens instruction text for a progress vertex name.
func truncateForName(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	const max = 48
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
