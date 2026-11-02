package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CM2-4: cmd/backtest needs the learned labels the live caps bucket by, read
// from a database it must not change. The read gets what the bot saved and
// leaves the file byte-for-byte as it was.
func TestReadLearnedLabelsIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")

	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SaveSectors(map[string]string{"SAP.DE": "technology"}); err != nil {
		t.Fatal(err)
	}

	if err := db.SaveAssetClasses(map[string]string{"IWDA.AS": "etf"}); err != nil {
		t.Fatal(err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	before, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)

	sectors, classes, err := ReadLearnedLabels(path)
	if err != nil {
		t.Fatal(err)
	}

	if sectors["SAP.DE"] != "technology" || classes["IWDA.AS"] != "etf" {
		t.Errorf("labels = %v / %v", sectors, classes)
	}

	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("reading the labels modified the database file")
	}

	if _, _, err := ReadLearnedLabels(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Error("a missing database read as empty rather than an error")
	}
}
