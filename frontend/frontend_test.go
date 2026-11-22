package frontend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A frontend/dist directory under the working directory wins over the
// embedded bundle; without one, the embedded bundle is served.
func TestFSPrefersDiskOverEmbedded(t *testing.T) {
	t.Run("disk", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "frontend", "dist"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "frontend", "dist", "index.html"), []byte("from disk"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)

		static, source, err := FS()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(source, "disk") {
			t.Fatalf("source = %q, want disk", source)
		}
		f, err := static.Open("index.html")
		if err != nil {
			t.Fatalf("opening index.html from disk: %v", err)
		}
		f.Close()
	})

	t.Run("embedded", func(t *testing.T) {
		t.Chdir(t.TempDir())
		static, source, err := FS()
		if err != nil {
			t.Fatal(err)
		}
		if source != "embedded" {
			t.Fatalf("source = %q, want embedded", source)
		}
		// Directories are never listed from the embedded bundle.
		if _, err := static.Open("/"); err == nil {
			t.Fatal("opening the embedded root directory succeeded, want not-exist")
		}
	})

	t.Run("dist is a file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "frontend"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "frontend", "dist"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if _, _, err := FS(); err == nil {
			t.Fatal("FS succeeded with frontend/dist as a file, want an error")
		}
	})
}
