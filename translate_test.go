package main

import (
	"encoding/json"
	"testing"
)

// Keys the upstream schema proto has no field for. Anything outside the
// normalizer's allow list must not survive, at any depth — a leak fails the
// whole request with "Unknown name ... Cannot find field".
var unsupportedSchemaKeys = []string{
	"const", "$schema", "$defs", "$ref", "title", "default", "examples",
	"additionalProperties", "allOf", "anyOf", "oneOf", "not",
}

func TestNormalizeJSONSchemaPrunesUnsupportedKeys(t *testing.T) {
	var schema map[string]any
	raw := `{
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object",
      "title": "Args",
      "additionalProperties": false,
      "properties": {
        "mode":    {"anyOf": [{"const": "moe", "type": "string"}, {"const": "dense"}]},
        "layers":  {"type": "integer", "default": 32, "minimum": 1},
        "targets": {"type": "array", "items": {"oneOf": [{"const": "vram", "title": "VRAM"}]}},
        "nested":  {"type": "object", "properties": {"deep": {"const": "x"}}}
      },
      "required": ["mode"]
    }`
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}

	normalized := normalizeJSONSchema(schema)

	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("normalized schema does not marshal: %v", err)
	}
	var walk func(any, string)
	walk = func(node any, path string) {
		switch n := node.(type) {
		case map[string]any:
			for k, v := range n {
				for _, banned := range unsupportedSchemaKeys {
					if k == banned && path != ".properties" {
						t.Errorf("unsupported key %q survived at %s: %s", k, path, encoded)
					}
				}
				walk(v, path+"."+k)
			}
		case []any:
			for i, v := range n {
				walk(v, path)
				_ = i
			}
		}
	}
	walk(normalized, "")

	props := normalized["properties"].(map[string]any)
	mode := props["mode"].(map[string]any)
	if got := mode["enum"]; got == nil {
		t.Errorf("const in an anyOf branch should become enum, got %v", mode)
	}
	if got, _ := mode["type"].(string); got != "STRING" {
		t.Errorf("type inside a branch should be uppercased, got %q", got)
	}
	if got, _ := normalized["type"].(string); got != "OBJECT" {
		t.Errorf("type = %q, want OBJECT", got)
	}
}

// Upstream rejects the whole request when an ARRAY reaches it without items,
// which tuple schemas (prefixItems, or items as a list) used to do.
func TestNormalizeJSONSchemaAlwaysGivesArraysItems(t *testing.T) {
	var schema map[string]any
	raw := `{
      "type": "object",
      "properties": {
        "where":   {"type": "array", "items": {"type": "array",
                    "prefixItems": [{"type": "string"}, {"type": "string"}, {}]}},
        "batch":   {"type": "array", "items": [{"type": "object"}]},
        "bare":    {"type": "array"},
        "refless": {"type": "array", "items": {"$ref": "#/$defs/Thing"}},
        "plain":   {"type": "array", "items": {"type": "string"}}
      }
    }`
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}

	props := normalizeJSONSchema(schema)["properties"].(map[string]any)

	var check func(node map[string]any, path string)
	check = func(node map[string]any, path string) {
		if t2, _ := node["type"].(string); t2 == "ARRAY" {
			items, ok := node["items"].(map[string]any)
			if !ok {
				t.Errorf("%s is an ARRAY without items: %v", path, node)
				return
			}
			check(items, path+".items")
		}
	}
	for name, sub := range props {
		check(sub.(map[string]any), name)
	}

	if got, _ := props["plain"].(map[string]any)["items"].(map[string]any)["type"].(string); got != "STRING" {
		t.Errorf("a normal array should keep its element type, got %q", got)
	}
	if got, _ := props["batch"].(map[string]any)["items"].(map[string]any)["type"].(string); got != "OBJECT" {
		t.Errorf("tuple-form items should adopt the first entry, got %q", got)
	}
}

func TestSanitizeSystemStripsAnthropicHeaders(t *testing.T) {
	in := "x-anthropic-billing-header: cc_version=2.1.273.b98; cc_entrypoint=sdk-cli;You are Claude Code."
	got := sanitizeSystem(in)
	for _, banned := range []string{"x-anthropic-billing-header", "cc_version", "cc_entrypoint", "Claude Code"} {
		if contains(got, banned) {
			t.Errorf("%q survived sanitizeSystem: %q", banned, got)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}()
}
