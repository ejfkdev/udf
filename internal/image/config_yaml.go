package image

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"udf/internal/types"
)

func WriteConfigYAML(outputDir string, meta *types.ImageMetadata) error {
	if meta == nil || meta.ConfigRaw == nil {
		return nil
	}

	root, err := buildYAMLNode(meta.ConfigRaw)
	if err != nil {
		return fmt.Errorf("build config yaml node: %w", err)
	}

	data, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal config yaml: %w", err)
	}

	targetPath := filepath.Join(outputDir, "config.yaml")
	if err := os.WriteFile(targetPath, data, 0o644); err != nil {
		return fmt.Errorf("write config yaml: %w", err)
	}

	return nil
}

func buildYAMLNode(v any) (*yaml.Node, error) {
	switch x := v.(type) {
	case map[string]any:
		node := &yaml.Node{Kind: yaml.MappingNode}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
			valueNode, err := buildYAMLNode(x[k])
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, keyNode, valueNode)
		}
		return node, nil
	case []any:
		node := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range x {
			child, err := buildYAMLNode(item)
			if err != nil {
				return nil, err
			}
			node.Content = append(node.Content, child)
		}
		return node, nil
	case string:
		return stringNode(x), nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(x)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(x)}, nil
	case int8, int16, int32, int64:
		return scalarNode("!!int", fmt.Sprintf("%d", x)), nil
	case uint, uint8, uint16, uint32, uint64:
		return scalarNode("!!int", fmt.Sprintf("%d", x)), nil
	case float32:
		return scalarNode("!!float", strconv.FormatFloat(float64(x), 'f', -1, 32)), nil
	case float64:
		return scalarNode("!!float", strconv.FormatFloat(x, 'f', -1, 64)), nil
	default:
		return scalarNode("!!str", fmt.Sprint(x)), nil
	}
}

func scalarNode(tag, value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

func stringNode(value string) *yaml.Node {
	normalized, multiline := normalizeString(value)
	node := &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: normalized,
	}
	if multiline {
		node.Style = yaml.LiteralStyle
	}
	return node
}

func normalizeString(value string) (string, bool) {
	s := strings.ReplaceAll(value, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	if strings.Contains(s, "\t") {
		s = formatTabbedString(s)
	}

	return s, strings.Contains(s, "\n")
}

func formatTabbedString(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		line = strings.ReplaceAll(line, "; \t", ";\n  ")
		line = strings.ReplaceAll(line, "\t\t", "    ")
		line = strings.ReplaceAll(line, "\t", "  ")
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}
