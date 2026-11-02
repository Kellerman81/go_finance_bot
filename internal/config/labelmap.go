package config

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// PatchLabelMap replaces config.yaml's top-level `sectors:` or
// `asset_classes:` block with m (the hand-written layer).
//
// These maps are keyed by symbol, and symbols carry dots (SAP.DE): Patch
// addresses a leaf by a dotted path, so SAP.DE resolved as SAP → DE and the
// maps were never written — the portfolio form applied a correction live,
// answered "saved", and the next restart reverted it (AP3-3). The block is
// rewritten whole instead: the comments inside it go, the rest of the file
// is untouched, and the result must read back as m and still load.
func PatchLabelMap(file, key string, m map[string]string) error {
	if key != "sectors" && key != "asset_classes" {
		return fmt.Errorf("PatchLabelMap: %q is not a symbol label map", key)
	}

	patchMu.Lock()
	defer patchMu.Unlock()

	src, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	// Unchanged: no write, so a portfolio save that did not touch the map
	// keeps the block's own formatting and comments.
	var cur map[string]map[string]string

	if yaml.Unmarshal(src, &cur) == nil && maps.Equal(cur[key], m) {
		return nil
	}

	out, err := replaceTopLevelBlock(src, key, m)
	if err != nil {
		return err
	}

	if err := verifyLabelMap(out, key, m); err != nil {
		return err
	}

	if err := stillLoads(src, out, file); err != nil {
		return err
	}

	return writeAtomic(file, out)
}

// verifyLabelMap checks the rewritten file reads back key as m.
func verifyLabelMap(out []byte, key string, m map[string]string) error {
	var back map[string]map[string]string

	if err := yaml.Unmarshal(out, &back); err != nil {
		return fmt.Errorf("verify %s: %w", key, err)
	}

	if !maps.Equal(back[key], m) && (len(back[key]) != 0 || len(m) != 0) {
		return fmt.Errorf("verify %s: the written block reads back as %v", key, back[key])
	}

	return nil
}

// replaceTopLevelBlock renders key: m and puts it where the file's top-level
// key block is — from its key line up to the next top-level key, leaving the
// comment and blank lines that precede that next key where they are — or
// appends it when the file has none.
func replaceTopLevelBlock(src []byte, key string, m map[string]string) ([]byte, error) {
	if m == nil {
		m = make(map[string]string)
	}

	block, err := yaml.Marshal(map[string]map[string]string{key: m})
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", key, err)
	}

	doc, err := parser.ParseBytes(src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	lines := lineStarts(src)

	start, end, found := topLevelSpan(doc, lines, src, key)
	if !found {
		out := append(bytes.Clone(bytes.TrimRight(src, "\n")), '\n', '\n')

		return append(out, block...), nil
	}

	out := make([]byte, 0, len(src)+len(block))

	out = append(out, src[:start]...)
	out = append(out, block...)

	return append(out, src[end:]...), nil
}

// topLevelSpan returns the byte span of key's top-level block.
func topLevelSpan(doc *ast.File, lines []int, src []byte, key string) (start, end int, ok bool) {
	if len(doc.Docs) == 0 {
		return 0, 0, false
	}

	body, isMap := doc.Docs[0].Body.(*ast.MappingNode)
	if !isMap {
		return 0, 0, false
	}

	for i, v := range body.Values {
		if v.Key.GetToken().Value != key {
			continue
		}

		start = lines[v.Key.GetToken().Position.Line-1]
		end = len(src)

		if i+1 < len(body.Values) {
			next := body.Values[i+1].Key.GetToken().Position.Line - 1

			// Leave the comment and blank lines above the next key to it.
			for next > 0 && lineIsTrivia(src, lines, next-1) && lines[next-1] > start {
				next--
			}

			end = lines[next]
		}

		return start, end, true
	}

	return 0, 0, false
}

// lineIsTrivia reports whether line i is blank or a column-0 comment.
func lineIsTrivia(src []byte, lines []int, i int) bool {
	lineEnd := len(src)
	if i+1 < len(lines) {
		lineEnd = lines[i+1]
	}

	text := strings.TrimRight(string(src[lines[i]:lineEnd]), "\r\n")

	return strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#")
}
