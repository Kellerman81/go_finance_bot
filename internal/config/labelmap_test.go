package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AP3-3: the label maps are keyed by dotted symbols the leaf patcher cannot
// address, so they are rewritten as a whole block. The rewrite must stay
// inside the block — the comment above the next key and the next key itself
// untouched — and an unchanged map must not rewrite anything.
func TestALabelMapRewriteStaysInsideItsBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "sectors:\n  OLD.DE: energy   # stale\n\n# exits: the money settings\nexits:\n  enabled: true\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PatchLabelMap(path, "sectors", map[string]string{"SAP.DE": "technology"}); err != nil {
		t.Fatal(err)
	}

	out, _ := os.ReadFile(path)
	got := string(out)

	if !strings.Contains(got, "SAP.DE: technology") || strings.Contains(got, "OLD.DE") {
		t.Errorf("the block was not replaced:\n%s", got)
	}

	if !strings.Contains(got, "# exits: the money settings\nexits:\n  enabled: true") {
		t.Errorf("the rewrite reached past its block:\n%s", got)
	}

	before := got
	if err := PatchLabelMap(path, "sectors", map[string]string{"SAP.DE": "technology"}); err != nil {
		t.Fatal(err)
	}

	if after, _ := os.ReadFile(path); string(after) != before {
		t.Errorf("an unchanged map rewrote the file:\n%s", after)
	}
}
