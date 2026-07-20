package corpus

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// SourceRule is one glob's ingestion rule (spec §3 source-map format):
// every SecLists file the glob matches is ingested as Type, tagged Tags,
// and — if FreqRank — treated as the primary commonality ranking signal
// (spec §4).
type SourceRule struct {
	Glob     string
	Type     TermType
	Tags     []string
	FreqRank bool
}

// SourceMap is an ordered list of rules, order preserved from the YAML
// document so ingestion (and its tests) are deterministic regardless of Go
// map iteration order.
type SourceMap struct {
	Rules []SourceRule
}

// LoadSourceMap reads and parses a sourcemap.yaml file from disk.
func LoadSourceMap(path string) (*SourceMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("corpus: read source map: %w", err)
	}
	return ParseSourceMap(data)
}

// ParseSourceMap decodes a source-map document (spec §3): a top-level YAML
// mapping of glob -> {type, tags, freq_rank}. Decoded via yaml.Node rather
// than a plain Go map so rule order matches the file's own order.
func ParseSourceMap(data []byte) (*SourceMap, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("corpus: parse source map: %w", err)
	}
	if len(doc.Content) == 0 {
		return &SourceMap{}, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("corpus: source map: expected a top-level mapping")
	}

	sm := &SourceMap{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		glob := root.Content[i].Value

		var raw struct {
			Type     string   `yaml:"type"`
			Tags     []string `yaml:"tags"`
			FreqRank bool     `yaml:"freq_rank"`
		}
		if err := root.Content[i+1].Decode(&raw); err != nil {
			return nil, fmt.Errorf("corpus: source map: glob %q: %w", glob, err)
		}
		typ, err := parseTermType(raw.Type)
		if err != nil {
			return nil, fmt.Errorf("corpus: source map: glob %q: %w", glob, err)
		}
		sm.Rules = append(sm.Rules, SourceRule{
			Glob: glob, Type: typ, Tags: raw.Tags, FreqRank: raw.FreqRank,
		})
	}
	return sm, nil
}

func parseTermType(s string) (TermType, error) {
	switch s {
	case "dir":
		return TypeDir, nil
	case "file":
		return TypeFile, nil
	case "stem":
		return TypeStem, nil
	case "fullpath":
		return TypeFullPath, nil
	default:
		return 0, fmt.Errorf("unknown type %q (want dir|file|stem|fullpath)", s)
	}
}
