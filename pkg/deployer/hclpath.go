package deployer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/pkg/errors"
	"github.com/zclconf/go-cty/cty"
)

// wildcard matches every key at the level it occupies.
const wildcard = "*"

// parseHCLValue decodes the raw HCL expression stored in a Terraform Cloud
// variable (e.g. `{ a = { image = "..." } }`) into a cty value.
//
// The expression is evaluated with a nil context, so it must be a literal:
// anything referencing a Terraform symbol has no meaning outside a plan and is
// rejected here rather than silently resolving to null.
func parseHCLValue(key, raw string) (cty.Value, error) {
	if strings.TrimSpace(raw) == "" {
		return cty.NilVal, fmt.Errorf(
			"variable %s has no readable value; sensitive variables are never returned by the Terraform Cloud API, so they cannot be updated by path",
			key,
		)
	}

	expr, diags := hclsyntax.ParseExpression([]byte(raw), key, hcl.InitialPos)
	if diags.HasErrors() {
		return cty.NilVal, errors.Wrapf(diags, "parsing variable %s as HCL", key)
	}

	val, diags := expr.Value(nil)
	if diags.HasErrors() {
		return cty.NilVal, errors.Wrapf(diags, "evaluating variable %s", key)
	}

	return val, nil
}

// renderHCLValue serializes a cty value back to formatted HCL, so a variable a
// human maintains in the Terraform Cloud UI stays readable after we write it.
func renderHCLValue(val cty.Value) string {
	tokens := hclwrite.TokensForValue(val)
	return strings.TrimSpace(string(hclwrite.Format(tokens.Bytes())))
}

// setPath replaces the string at path within val, and reports every concrete
// path it wrote. A "*" segment fans out across all keys at that level.
//
// This is replace-only: every segment must already exist, and the leaf must
// already be a string. A path that matches nothing is an error, not a no-op —
// a deploy that silently updates zero heralds is worse than a failed one. The
// leaf type check is what stops a truncated path (`a` instead of `a.image`)
// from overwriting a whole object with a bare image string.
func setPath(val cty.Value, path []string, leaf string) (cty.Value, []string, error) {
	if len(path) == 0 {
		if val.Type() != cty.String {
			return cty.NilVal, nil, fmt.Errorf(
				"refusing to overwrite a %s with a string; the path must address a string",
				val.Type().FriendlyName(),
			)
		}
		return cty.StringVal(leaf), []string{""}, nil
	}

	if val.IsNull() || !val.CanIterateElements() {
		return cty.NilVal, nil, fmt.Errorf(
			"cannot descend into a %s",
			val.Type().FriendlyName(),
		)
	}

	elems := val.AsValueMap()
	segment, rest := path[0], path[1:]

	keys := make([]string, 0, len(elems))
	if segment == wildcard {
		for k := range elems {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	} else {
		if _, ok := elems[segment]; !ok {
			return cty.NilVal, nil, fmt.Errorf("key %q does not exist", segment)
		}
		keys = append(keys, segment)
	}

	if len(keys) == 0 {
		return cty.NilVal, nil, errors.New("matched no keys")
	}

	// Rebuilt as an object regardless of whether the source was an object or a
	// map: the result is re-parsed by Terraform against the variable's declared
	// type, so the distinction does not survive the round trip anyway.
	updated := make(map[string]cty.Value, len(elems))
	for k, v := range elems {
		updated[k] = v
	}

	written := make([]string, 0, len(keys))
	for _, k := range keys {
		child, childWritten, err := setPath(elems[k], rest, leaf)
		if err != nil {
			return cty.NilVal, nil, errors.Wrapf(err, "at %q", k)
		}
		updated[k] = child
		for _, w := range childWritten {
			written = append(written, strings.TrimSuffix(k+"."+w, "."))
		}
	}

	return cty.ObjectVal(updated), written, nil
}

// splitPath turns "a.image" into ["a", "image"].
func splitPath(path string) ([]string, error) {
	segments := strings.Split(path, ".")
	for _, s := range segments {
		if s == "" {
			return nil, fmt.Errorf("path %q has an empty segment", path)
		}
	}
	return segments, nil
}
