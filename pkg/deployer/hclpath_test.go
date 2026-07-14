package deployer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The herald roster this was built for: a map of name -> { image, archil_disk }.
const roster = `{
  a = {
    image       = "ghcr.io/xmtplabs/herald-lite:sha-7036d9b"
    archil_disk = "herald-dev/herald-a"
  }
  b = {
    image       = "ghcr.io/xmtplabs/herald-lite:sha-7036d9b"
    archil_disk = "herald-dev/herald-b"
  }
}`

// roster3 gives the wildcard cases a third herald to fan out to, so a "*" is
// distinguishable from a two-key wave.
const roster3 = `{
  a = {
    image       = "old-a"
    archil_disk = "herald-dev/herald-a"
  }
  b = {
    image       = "old-b"
    archil_disk = "herald-dev/herald-b"
  }
  c = {
    image       = "old-c"
    archil_disk = "herald-dev/herald-c"
  }
}`

// field reads one attribute of every herald back out of rendered HCL, so a test
// asserts on the parsed result rather than on formatting.
func field(t *testing.T, rendered, attr string) map[string]string {
	t.Helper()

	val, err := parseHCLValue("var", rendered)
	require.NoError(t, err)

	out := map[string]string{}
	for name, herald := range val.AsValueMap() {
		out[name] = herald.GetAttr(attr).AsString()
	}
	return out
}

func imagesOf(t *testing.T, rendered string) map[string]string {
	t.Helper()
	return field(t, rendered, "image")
}

func disksOf(t *testing.T, rendered string) map[string]string {
	t.Helper()
	return field(t, rendered, "archil_disk")
}

// set applies one path update to raw and returns the re-rendered HCL.
func set(t *testing.T, raw, path, value string) (string, []string) {
	t.Helper()

	val, err := parseHCLValue("var", raw)
	require.NoError(t, err)

	segments, err := splitPath(path)
	require.NoError(t, err)

	next, written, err := setPath(val, segments, value)
	require.NoError(t, err)

	return renderHCLValue(next), written
}

func TestSetPath_SingleKey(t *testing.T) {
	out, written := set(t, roster, "a.image", "ghcr.io/xmtplabs/herald-lite@sha256:beef")

	require.Equal(t, []string{"a.image"}, written)
	// a moved...
	require.Contains(t, out, `image       = "ghcr.io/xmtplabs/herald-lite@sha256:beef"`)
	// ...b did not, and neither disk was touched.
	require.Contains(t, out, `"ghcr.io/xmtplabs/herald-lite:sha-7036d9b"`)
	require.Contains(t, out, `archil_disk = "herald-dev/herald-a"`)
	require.Contains(t, out, `archil_disk = "herald-dev/herald-b"`)
}

func TestSetPath_Wildcard(t *testing.T) {
	out, written := set(t, roster, "*.image", "ghcr.io/xmtplabs/herald-lite@sha256:beef")

	require.Equal(t, []string{"a.image", "b.image"}, written)
	require.NotContains(t, out, "sha-7036d9b")
	require.Contains(t, out, `archil_disk = "herald-dev/herald-a"`)
	require.Contains(t, out, `archil_disk = "herald-dev/herald-b"`)
}

// The output has to survive being read back in, or the next deploy cannot parse
// what this one wrote.
func TestSetPath_RoundTrips(t *testing.T) {
	once, _ := set(t, roster, "*.image", "ghcr.io/xmtplabs/herald-lite@sha256:beef")
	twice, _ := set(t, once, "*.image", "ghcr.io/xmtplabs/herald-lite@sha256:cafe")

	require.Contains(t, twice, "sha256:cafe")
	require.NotContains(t, twice, "sha256:beef")
	require.Contains(t, twice, `archil_disk = "herald-dev/herald-a"`)
}

// A JSON object is a valid HCL object expression, so a value written by hand in
// either dialect has to parse.
func TestSetPath_AcceptsJSONDialect(t *testing.T) {
	out, written := set(t, `{"a": {"image": "old", "archil_disk": "d"}}`, "a.image", "new")

	require.Equal(t, []string{"a.image"}, written)
	require.Contains(t, out, `image       = "new"`)
	require.Contains(t, out, `archil_disk = "d"`)
}

func TestSetPath_ReplaceOnly(t *testing.T) {
	val, err := parseHCLValue("var", roster)
	require.NoError(t, err)

	// A herald that does not exist is not created: the Archil disk behind it has
	// to be provisioned out of band first, so a missing key is a typo.
	segments, err := splitPath("zz.image")
	require.NoError(t, err)
	_, _, err = setPath(val, segments, "img")
	require.ErrorContains(t, err, `key "zz" does not exist`)

	// Nor is a new field grafted onto an existing herald.
	segments, err = splitPath("a.tag")
	require.NoError(t, err)
	_, _, err = setPath(val, segments, "img")
	require.ErrorContains(t, err, `key "tag" does not exist`)
}

// A truncated path ("a" rather than "a.image") would otherwise flatten a whole
// herald object into a bare image string, and Terraform would only reject it at
// plan time, after the variable had already been clobbered.
func TestSetPath_RefusesToOverwriteAnObject(t *testing.T) {
	val, err := parseHCLValue("var", roster)
	require.NoError(t, err)

	segments, err := splitPath("a")
	require.NoError(t, err)

	_, _, err = setPath(val, segments, "img")
	require.ErrorContains(t, err, "refusing to overwrite a object")
}

func TestSetPath_WildcardMatchingNothingIsAnError(t *testing.T) {
	val, err := parseHCLValue("var", `{}`)
	require.NoError(t, err)

	segments, err := splitPath("*.image")
	require.NoError(t, err)

	// Not a no-op: a deploy that quietly rolls zero heralds is worse than one
	// that fails.
	_, _, err = setPath(val, segments, "img")
	require.ErrorContains(t, err, "matched no keys")
}

// Terraform Cloud never returns the value of a sensitive variable, so there is
// nothing to merge into and the failure has to be legible.
func TestParseHCLValue_SensitiveVariable(t *testing.T) {
	_, err := parseHCLValue("herald_roster", "")
	require.ErrorContains(t, err, "sensitive variables are never returned")
}

func TestParseHCLValue_NotALiteral(t *testing.T) {
	_, err := parseHCLValue("var", `{ a = var.something }`)
	require.Error(t, err)
}

func TestSplitPath_EmptySegment(t *testing.T) {
	_, err := splitPath("a..image")
	require.ErrorContains(t, err, "empty segment")
}
