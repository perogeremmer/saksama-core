package cli

import "testing"

// The test binary is built from a working tree, so its module version is
// "(devel)": these cases pin what happens when the fallback has nothing to
// offer, and that an injected value is never second-guessed.

func TestBuildVersionPrefersInjected(t *testing.T) {
	cases := []struct {
		name     string
		injected string
		want     string
	}{
		{"goreleaser shape", "0.2.0", "0.2.0"},
		{"git describe shape", "v0.2.0", "v0.2.0"},
		{"dirty tree", "v0.2.0-dirty", "v0.2.0-dirty"},
		{"snapshot", "0.2.0-next", "0.2.0-next"},
	}
	for _, c := range cases {
		if got := buildVersion(c.injected); got != c.want {
			t.Errorf("%s: buildVersion(%q) = %q, want %q", c.name, c.injected, got, c.want)
		}
	}
}

func TestBuildVersionUnstampedIsDev(t *testing.T) {
	// No ldflags and an untagged tree: "dev" is the answer, not "" and not a
	// fabricated number.
	for _, injected := range []string{"dev", ""} {
		if got := buildVersion(injected); got != "dev" {
			t.Errorf("buildVersion(%q) = %q, want dev", injected, got)
		}
	}
}

func TestUsableVersion(t *testing.T) {
	cases := map[string]string{
		"v0.2.0":       "0.2.0", // go install pkg@v0.2.0
		"v0.2.0+dirty": "0.2.0+dirty",
		"0.2.0":        "0.2.0", // already goreleaser shape
		"v0.2.0-rc.1":  "0.2.0-rc.1",
		"(devel)":      "", // go build in a working tree
		"":             "",
	}
	for in, want := range cases {
		if got := usableVersion(in); got != want {
			t.Errorf("usableVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
