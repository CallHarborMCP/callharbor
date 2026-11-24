package web

import (
	"io/fs"
	"testing"
)

func TestAssetsExposeEmbeddedDirectory(t *testing.T) {
	assets, err := Assets()
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	if _, err := fs.Stat(assets, ".gitkeep"); err != nil {
		t.Fatalf("embedded directory missing .gitkeep: %v", err)
	}
}

func TestBuiltAssetsContainIndexWhenSynced(t *testing.T) {
	assets, err := Assets()
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		t.Skip("web UI not built; run scripts/build-web.sh")
	}
	data, err := fs.ReadFile(assets, "index.html")
	if err != nil || len(data) == 0 {
		t.Fatalf("embedded index.html unreadable or empty: %v", err)
	}
}
