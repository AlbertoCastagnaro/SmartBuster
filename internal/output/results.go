// Package output renders confirmed Findings as a tree, a flat list, plain
// text, and JSON (spec §12). Findings arriving here are assumed already
// deduplicated by content hash with aliases attached — that invariant is
// maintained by the coordinator (internal/engine), not redone here.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
)

// TreeNode is one path segment in the discovered-path tree. Finding is nil
// for an intermediate segment that has no confirmed finding of its own.
type TreeNode struct {
	Name     string
	Finding  *engine.Finding
	Children map[string]*TreeNode
}

// BuildTree nests findings by URL path into a tree rooted at "/".
func BuildTree(findings []engine.Finding) *TreeNode {
	root := &TreeNode{Name: "/", Children: map[string]*TreeNode{}}
	for i := range findings {
		f := &findings[i]
		segs := strings.Split(strings.Trim(pathOf(f.URL), "/"), "/")
		node := root
		for _, seg := range segs {
			if seg == "" {
				continue
			}
			child, ok := node.Children[seg]
			if !ok {
				child = &TreeNode{Name: seg, Children: map[string]*TreeNode{}}
				node.Children[seg] = child
			}
			node = child
		}
		node.Finding = f
	}
	return root
}

func pathOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Path
}

// WriteTree renders the discovered-path tree as indented text.
func WriteTree(w io.Writer, findings []engine.Finding) error {
	return writeTreeNode(w, BuildTree(findings), 0)
}

func writeTreeNode(w io.Writer, node *TreeNode, depth int) error {
	names := make([]string, 0, len(node.Children))
	for name := range node.Children {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		child := node.Children[name]
		line := strings.Repeat("  ", depth) + name
		if child.Finding != nil {
			line += formatSuffix(*child.Finding)
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return err
		}
		if err := writeTreeNode(w, child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// FlatList returns findings sorted by URL, for exports.
func FlatList(findings []engine.Finding) []engine.Finding {
	out := append([]engine.Finding(nil), findings...)
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

// WritePlaintext renders gobuster-style lines for pipeline use:
// PATH (Status: N) [Size: B] [conf: 0.xx]
func WritePlaintext(w io.Writer, findings []engine.Finding) error {
	for _, f := range FlatList(findings) {
		if _, err := io.WriteString(w, f.URL+formatSuffix(f)+"\n"); err != nil {
			return err
		}
	}
	return nil
}

func formatSuffix(f engine.Finding) string {
	return fmt.Sprintf(" (Status: %d) [Size: %d] [conf: %.2f]", f.Status, f.Size, f.Confidence)
}

// WriteJSON exports findings as an indented JSON array.
func WriteJSON(w io.Writer, findings []engine.Finding) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(FlatList(findings))
}
