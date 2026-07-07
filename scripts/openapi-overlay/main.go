// Command openapi-overlay deep-merges an overlay YAML document into a base
// YAML document, in place. It exists to patch hand-maintained operations into
// the generated openapi.yaml where apispec cannot extract them (see
// docs/openapi-generation.md).
//
// Merge semantics: mappings merge recursively with overlay values winning;
// scalars and sequences are replaced wholesale. Base key order is preserved;
// overlay-only keys append.
//
// Usage: openapi-overlay <base.yaml> <overlay.yaml>
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

func mergeNodes(base, overlay *yaml.Node) {
	if base.Kind != yaml.MappingNode || overlay.Kind != yaml.MappingNode {
		*base = *overlay
		return
	}
	for i := 0; i+1 < len(overlay.Content); i += 2 {
		key, val := overlay.Content[i], overlay.Content[i+1]
		if isNull(val) {
			deleteKey(base, key.Value)
			continue
		}
		if existing := mappingValue(base, key.Value); existing != nil {
			mergeNodes(existing, val)
			continue
		}
		base.Content = append(base.Content, key, val)
	}
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

func deleteKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func run(basePath, overlayPath string) error {
	baseDoc, err := parseFile(basePath)
	if err != nil {
		return err
	}
	overlayDoc, err := parseFile(overlayPath)
	if err != nil {
		return err
	}
	mergeNodes(baseDoc, overlayDoc)

	// Encode fully in memory before touching the base file, so a mid-encode
	// failure can never leave a truncated openapi.yaml behind.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(baseDoc); err != nil {
		return fmt.Errorf("marshal merged doc: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("marshal merged doc: %w", err)
	}
	if err := os.WriteFile(basePath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", basePath, err)
	}
	return nil
}

func parseFile(path string) (*yaml.Node, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: multiple YAML documents are not supported", path)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("parse %s: empty document", path)
	}
	// Anchors/aliases don't survive the merge: replacing or deleting the
	// subtree that defines an anchor would leave dangling aliases and emit
	// unparseable YAML. Neither the generated spec nor the overlay uses them.
	if node := findAnchorOrAlias(doc.Content[0]); node != nil {
		return nil, fmt.Errorf("parse %s: anchors/aliases are not supported (line %d)", path, node.Line)
	}
	return doc.Content[0], nil
}

func findAnchorOrAlias(n *yaml.Node) *yaml.Node {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return n
	}
	for _, child := range n.Content {
		if found := findAnchorOrAlias(child); found != nil {
			return found
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: openapi-overlay <base.yaml> <overlay.yaml>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
