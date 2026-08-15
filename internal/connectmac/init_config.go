package connectmac

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

const DefaultConnectMacServer = "https://cm.hsgitlab.xyz"

type initConfigDocument struct {
	original []byte
	root     yaml.Node
	changed  bool
}

func newInitConfigDocument(data []byte) (*initConfigDocument, error) {
	doc := &initConfigDocument{original: append([]byte(nil), data...)}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &doc.root); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if doc.root.Kind == 0 {
		doc.root = yaml.Node{
			Kind: yaml.DocumentNode,
			Content: []*yaml.Node{{
				Kind: yaml.MappingNode,
				Tag:  "!!map",
			}},
		}
	}
	if doc.root.Kind != yaml.DocumentNode || len(doc.root.Content) != 1 || doc.root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse config: top level must be a mapping")
	}
	return doc, nil
}

func (d *initConfigDocument) ServerUserAPI() string {
	return d.value("server", "user_api")
}

func (d *initConfigDocument) ServerToken() string {
	return d.value("server", "token")
}

func (d *initConfigDocument) DefaultUser() string {
	return d.value("defaults", "user")
}

func (d *initConfigDocument) DefaultIdentityFile() string {
	return d.value("defaults", "identity_file")
}

func (d *initConfigDocument) SetServerUserAPI(value string) {
	d.set(value, "server", "user_api")
}

func (d *initConfigDocument) SetServerToken(value string) {
	d.set(value, "server", "token")
}

func (d *initConfigDocument) SetDefaultUser(value string) {
	d.set(value, "defaults", "user")
}

func (d *initConfigDocument) SetDefaultIdentityFile(value string) {
	d.set(value, "defaults", "identity_file")
}

func (d *initConfigDocument) Bytes() ([]byte, bool, error) {
	if !d.changed {
		return append([]byte(nil), d.original...), false, nil
	}
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&d.root); err != nil {
		return nil, false, fmt.Errorf("encode config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, false, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), true, nil
}

func (d *initConfigDocument) value(path ...string) string {
	node := d.mappingValue(path...)
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func (d *initConfigDocument) set(value string, path ...string) {
	if value == "" || len(path) == 0 {
		return
	}
	if current := d.mappingValue(path...); current != nil && current.Kind == yaml.ScalarNode && current.Value == value {
		return
	}
	mapping := d.root.Content[0]
	for _, key := range path[:len(path)-1] {
		next := mappingEntry(mapping, key)
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			appendMappingEntry(mapping, key, next)
		} else if next.Kind != yaml.MappingNode {
			return
		}
		mapping = next
	}
	key := path[len(path)-1]
	if current := mappingEntry(mapping, key); current != nil {
		current.Kind = yaml.ScalarNode
		current.Tag = "!!str"
		current.Value = value
		current.Content = nil
		current.Alias = nil
	} else {
		appendMappingEntry(mapping, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	d.changed = true
}

func (d *initConfigDocument) mappingValue(path ...string) *yaml.Node {
	if d.root.Kind != yaml.DocumentNode || len(d.root.Content) != 1 {
		return nil
	}
	current := d.root.Content[0]
	for _, key := range path {
		if current.Kind != yaml.MappingNode {
			return nil
		}
		current = mappingEntry(current, key)
		if current == nil {
			return nil
		}
	}
	return current
}

func mappingEntry(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func appendMappingEntry(mapping *yaml.Node, key string, value *yaml.Node) {
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

func writePrivateFileAtomic(path string, data []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return fmt.Errorf("secure parent directory: %w", err)
	}

	temp, err := os.CreateTemp(parent, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary file: %w", err)
	}
	closed = true
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace destination: %w", err)
	}
	return nil
}
