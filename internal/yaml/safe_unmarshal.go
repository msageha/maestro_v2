package yaml

import (
	"fmt"

	yamlv3 "gopkg.in/yaml.v3"
)

// Anchor/alias safety limits to prevent resource exhaustion from
// exponential alias expansion (billion laughs / YAML bomb attacks).
const (
	// MaxAnchors is the maximum number of anchors allowed in a single YAML document.
	MaxAnchors = 100

	// MaxAliasDepth is the maximum nesting depth of alias references.
	// An alias that references an anchor containing another alias counts as depth+1.
	MaxAliasDepth = 10
)

// SafeUnmarshal unmarshals YAML content into out, applying anchor count and
// alias depth limits before the full decode. This prevents resource exhaustion
// from exponential alias expansion (billion laughs attack).
//
// The function first decodes the YAML into a yaml.Node tree to inspect the
// structure, then unmarshals into the target type only if limits are satisfied.
func SafeUnmarshal(data []byte, out any) error {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(data, &doc); err != nil {
		return err
	}

	anchors := make(map[string]*yamlv3.Node)
	if err := collectAnchors(&doc, anchors); err != nil {
		return err
	}

	if err := checkAliasDepth(&doc, 0); err != nil {
		return err
	}

	return yamlv3.Unmarshal(data, out)
}

// collectAnchors walks the node tree and collects all anchors, returning
// an error if the count exceeds MaxAnchors.
func collectAnchors(node *yamlv3.Node, anchors map[string]*yamlv3.Node) error {
	if node == nil {
		return nil
	}

	if node.Anchor != "" {
		anchors[node.Anchor] = node
		if len(anchors) > MaxAnchors {
			return fmt.Errorf("yaml safety: anchor count %d exceeds maximum of %d", len(anchors), MaxAnchors)
		}
	}

	for _, child := range node.Content {
		if err := collectAnchors(child, anchors); err != nil {
			return err
		}
	}
	return nil
}

// checkAliasDepth walks the node tree and checks that alias references do not
// exceed MaxAliasDepth levels of nesting.
func checkAliasDepth(node *yamlv3.Node, depth int) error {
	if node == nil {
		return nil
	}

	if node.Kind == yamlv3.AliasNode {
		depth++
		if depth > MaxAliasDepth {
			return fmt.Errorf("yaml safety: alias depth %d exceeds maximum of %d", depth, MaxAliasDepth)
		}
		// Follow the alias to its target and check depth recursively.
		if node.Alias != nil {
			if err := checkAliasDepth(node.Alias, depth); err != nil {
				return err
			}
		}
		return nil
	}

	for _, child := range node.Content {
		if err := checkAliasDepth(child, depth); err != nil {
			return err
		}
	}
	return nil
}
