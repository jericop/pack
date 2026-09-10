package multiplatform

// NFR-3: integration tests (real extension, both arches) are a tracked follow-up.

import (
	"testing"

	h "github.com/buildpacks/pack/testhelpers"
)

func TestParseExtensionDockerfile(t *testing.T) {
	t.Run("accepts the CNB-allowed instruction set", func(t *testing.T) {
		src := []byte(`
ARG base_image
FROM ${base_image}
ARG build_id=0
ENV FOO=bar
LABEL io.buildpacks.rebasable=true
USER 0:0
WORKDIR /tmp
SHELL ["/bin/bash", "-c"]
RUN echo hello
COPY thing /thing
ADD other /other
`)
		df, err := parseExtensionDockerfile(src)
		h.AssertNil(t, err)
		// FROM + 2 ARG + ENV + LABEL + USER + WORKDIR + SHELL + RUN + COPY + ADD = 11
		h.AssertEq(t, len(df.instructions), 11)
	})

	t.Run("rejects a disallowed instruction", func(t *testing.T) {
		for _, verb := range []string{"VOLUME /data", "ENTRYPOINT [\"/x\"]", "CMD [\"/x\"]", "EXPOSE 80", "HEALTHCHECK NONE"} {
			_, err := parseExtensionDockerfile([]byte("FROM ${base_image}\n" + verb + "\n"))
			h.AssertNotNil(t, err)
			h.AssertContains(t, err.Error(), "is not permitted")
		}
	})

	t.Run("rejects a second FROM", func(t *testing.T) {
		_, err := parseExtensionDockerfile([]byte("FROM ${base_image}\nFROM scratch\n"))
		h.AssertNotNil(t, err)
		h.AssertContains(t, err.Error(), "multiple FROM")
	})

	t.Run("rejects a non-ARG instruction before FROM", func(t *testing.T) {
		_, err := parseExtensionDockerfile([]byte("RUN echo too-early\nFROM ${base_image}\n"))
		h.AssertNotNil(t, err)
		h.AssertContains(t, err.Error(), "before FROM")
	})

	t.Run("allows ARG before FROM", func(t *testing.T) {
		_, err := parseExtensionDockerfile([]byte("ARG base_image\nFROM ${base_image}\n"))
		h.AssertNil(t, err)
	})

	t.Run("errors when there is no FROM", func(t *testing.T) {
		// Only ARG (allowed before FROM) so we reach the no-FROM check rather than the
		// before-FROM check.
		_, err := parseExtensionDockerfile([]byte("ARG base_image\nARG build_id=0\n"))
		h.AssertNotNil(t, err)
		h.AssertContains(t, err.Error(), "no FROM")
	})

	t.Run("joins line continuations", func(t *testing.T) {
		df, err := parseExtensionDockerfile([]byte("FROM ${base_image}\nRUN echo a \\\n  && echo b\n"))
		h.AssertNil(t, err)
		h.AssertEq(t, len(df.instructions), 2)
		h.AssertContains(t, df.instructions[1].args, "echo a")
		h.AssertContains(t, df.instructions[1].args, "echo b")
	})

	t.Run("ignores comments and blank lines", func(t *testing.T) {
		df, err := parseExtensionDockerfile([]byte("# a comment\n\nFROM ${base_image}\n# another\nRUN echo hi\n"))
		h.AssertNil(t, err)
		h.AssertEq(t, len(df.instructions), 2)
	})
}

func TestInterpolate(t *testing.T) {
	args := map[string]string{"base_image": "run:1", "build_id": "uuid-123", "user_id": "1000"}
	env := map[string]string{"FOO": "bar"}

	h.AssertEq(t, interpolate("$base_image", args, env), "run:1")
	h.AssertEq(t, interpolate("${base_image}", args, env), "run:1")
	h.AssertEq(t, interpolate("uid=$user_id", args, env), "uid=1000")
	h.AssertEq(t, interpolate("$FOO", args, env), "bar")     // falls through to env
	h.AssertEq(t, interpolate("$missing", args, env), "")    // unknown => empty
	h.AssertEq(t, interpolate("$$literal", args, env), "$literal")
	h.AssertEq(t, interpolate("a-${build_id}-b", args, env), "a-uuid-123-b")
}

func TestReferencesVar(t *testing.T) {
	h.AssertEq(t, referencesVar("echo $build_id", "build_id"), true)
	h.AssertEq(t, referencesVar("echo ${build_id}", "build_id"), true)
	h.AssertEq(t, referencesVar("echo hi", "build_id"), false)
}

func TestParseKeyValues(t *testing.T) {
	t.Run("multiple key=value pairs", func(t *testing.T) {
		kv := parseKeyValues(`A=1 B=two C="three four"`)
		h.AssertEq(t, kv["A"], "1")
		h.AssertEq(t, kv["B"], "two")
		h.AssertEq(t, kv["C"], "three four")
	})
	t.Run("legacy KEY value single-pair form", func(t *testing.T) {
		kv := parseKeyValues(`io.buildpacks.rebasable true`)
		h.AssertEq(t, kv["io.buildpacks.rebasable"], "true")
	})
}

func TestParseCopyArgs(t *testing.T) {
	t.Run("single source + dest", func(t *testing.T) {
		src, dst, err := parseCopyArgs("thing /dest/thing")
		h.AssertNil(t, err)
		h.AssertEq(t, len(src), 1)
		h.AssertEq(t, src[0], "thing")
		h.AssertEq(t, dst, "/dest/thing")
	})
	t.Run("multiple sources", func(t *testing.T) {
		src, dst, err := parseCopyArgs("a b c /dest/")
		h.AssertNil(t, err)
		h.AssertEq(t, len(src), 3)
		h.AssertEq(t, dst, "/dest/")
	})
	t.Run("ignores --chown but rejects --from", func(t *testing.T) {
		_, _, err := parseCopyArgs("--chown=1000:1000 a /b")
		h.AssertNil(t, err)
		_, _, err = parseCopyArgs("--from=builder a /b")
		h.AssertNotNil(t, err)
		h.AssertContains(t, err.Error(), "--from")
	})
	t.Run("errors without a destination", func(t *testing.T) {
		_, _, err := parseCopyArgs("onlyone")
		h.AssertNotNil(t, err)
	})
}

func TestParseJSONArray(t *testing.T) {
	t.Run("exec form", func(t *testing.T) {
		arr, ok := parseJSONArray(`["/bin/bash", "-c", "echo hi"]`)
		h.AssertEq(t, ok, true)
		h.AssertEq(t, len(arr), 3)
		h.AssertEq(t, arr[0], "/bin/bash")
		h.AssertEq(t, arr[2], "echo hi")
	})
	t.Run("shell form is not an array", func(t *testing.T) {
		_, ok := parseJSONArray(`echo hi`)
		h.AssertEq(t, ok, false)
	})
}

func TestRunArgv(t *testing.T) {
	shell := []string{"/bin/sh", "-c"}
	args := map[string]string{"build_id": "uuid-9"}
	env := map[string]string{}

	t.Run("shell form wraps with SHELL and interpolates", func(t *testing.T) {
		argv, bid := runArgv("echo $build_id", shell, args, env)
		h.AssertEq(t, len(argv), 3)
		h.AssertEq(t, argv[0], "/bin/sh")
		h.AssertEq(t, argv[2], "echo uuid-9")
		h.AssertEq(t, bid, true)
	})
	t.Run("exec form used verbatim, no build_id", func(t *testing.T) {
		argv, bid := runArgv(`["/bin/echo","hi"]`, shell, args, env)
		h.AssertEq(t, len(argv), 2)
		h.AssertEq(t, argv[0], "/bin/echo")
		h.AssertEq(t, bid, false)
	})
}
