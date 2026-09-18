package configs

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EditableKey describes one config path that `gamelist config set` may write.
type EditableKey struct {
	Key  string
	Kind string // "bool" or "string"
}

// EditableKeys lists every settable config key, in display order.
func EditableKeys() []EditableKey {
	return []EditableKey{
		{"igdb.clientId", "string"},
		{"igdb.clientSecret", "string"},
		{"database.dataSourceName", "string"},
		{"stores.steam.enabled", "bool"},
		{"stores.steam.apiKey", "string"},
		{"stores.steam.steamId", "string"},
		{"stores.epic.enabled", "bool"},
		{"stores.epic.clientId", "string"},
		{"stores.epic.clientSecret", "string"},
		{"stores.gog.enabled", "bool"},
		{"stores.ubisoft.enabled", "bool"},
		{"stores.xbox.enabled", "bool"},
		{"stores.battlenet.enabled", "bool"},
		{"stores.battlenet.clientId", "string"},
		{"stores.battlenet.clientSecret", "string"},
		{"stores.dlsite.enabled", "bool"},
	}
}

// LookupKeyKind returns the value kind ("bool"/"string") for a key, or ""
// when the key is not editable.
func LookupKeyKind(key string) string {
	for _, k := range EditableKeys() {
		if k.Key == key {
			return k.Kind
		}
	}
	return ""
}

// SetYAMLValues updates dotted keys in the YAML file at configPath while
// preserving comments and key order (yaml.Node round-trip). Missing
// intermediate keys are created.
func SetYAMLValues(configPath string, updates map[string]string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0] == nil {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}

	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := setPath(root, strings.Split(key, "."), updates[key]); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	enc.Close()
	if err := os.WriteFile(configPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// setPath walks (creating as needed) to the dotted path inside mapNode and
// sets the final scalar.
func setPath(node *yaml.Node, parts []string, value string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping while walking %q", parts[0])
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == parts[0] {
			target := node.Content[i+1]
			if len(parts) == 1 {
				return setScalar(target, value)
			}
			return setPath(target, parts[1:], value)
		}
	}

	// Key does not exist yet: create it.
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: parts[0]}
	if len(parts) == 1 {
		valNode := &yaml.Node{}
		if err := setScalar(valNode, value); err != nil {
			return err
		}
		node.Content = append(node.Content, keyNode, valNode)
		return nil
	}
	child := &yaml.Node{Kind: yaml.MappingNode}
	node.Content = append(node.Content, keyNode, child)
	return setPath(child, parts[1:], value)
}

// setScalar writes value into node, preserving any attached comments.
// Booleans are normalized so `yes`/`1`/`on` become real YAML bools.
func setScalar(node *yaml.Node, value string) error {
	if node.Kind != yaml.ScalarNode {
		// Replace a nested mapping/sequence wholesale; config values are scalars.
		*node = yaml.Node{Kind: yaml.ScalarNode}
	}
	if b, ok := parseBoolValue(value); ok {
		node.Tag = "!!bool"
		node.Value = b
	} else {
		node.Tag = "!!str"
		node.Value = value
		node.Style = 0 // let the encoder quote when the value needs it
	}
	return nil
}

func parseBoolValue(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return "true", true
	case "false", "0", "no", "off":
		return "false", true
	}
	return "", false
}
