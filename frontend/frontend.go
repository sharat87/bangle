// Package frontend serves the built web UI (frontend/dist, written by
// build.mjs).
//
// The bundle is embedded into the binary, but a dist directory on disk wins
// when there is one at frontend/dist relative to the working directory: that
// is the case for `go run` from the repo (`make`), so a frontend
// change is `node build.mjs` and a reload, with no server restart. A binary
// run anywhere else serves its embedded copy.
//
// frontend/dist/.gitkeep is committed so the embed pattern always matches
// and the package compiles before the first frontend build. A build without
// a bundle still compiles; the server answers "frontend not built" instead.
package frontend

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// diskDir is where `go run` from the repo root finds the live bundle.
const diskDir = "frontend/dist"

// FS returns the filesystem to serve /static/ and index.html from, and a
// description of which one it is, for the startup log.
func FS() (http.FileSystem, string, error) {
	fi, err := os.Stat(diskDir)
	switch {
	case err == nil && fi.IsDir():
		abs, err := filepath.Abs(diskDir)
		if err != nil {
			return nil, "", fmt.Errorf("resolving %s: %w", diskDir, err)
		}
		return http.Dir(diskDir), "disk (" + abs + ")", nil
	case err == nil:
		return nil, "", fmt.Errorf("%s exists but is not a directory", diskDir)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, "", fmt.Errorf("checking %s: %w", diskDir, err)
	}
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, "", fmt.Errorf("opening embedded bundle: %w", err)
	}
	return filesOnly{http.FS(sub)}, "embedded", nil
}

// filesOnly is the embedded view of the bundle: no directory listings
// (http.FileServer renders one for any directory without an index.html) and
// no source maps (the build emits main.js.map for local debugging; a shipped
// binary has no use for it).
type filesOnly struct{ fs http.FileSystem }

func (f filesOnly) Open(name string) (http.File, error) {
	if strings.HasSuffix(name, ".map") {
		return nil, fs.ErrNotExist
	}
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	fi, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if fi.IsDir() {
		file.Close()
		return nil, fs.ErrNotExist
	}
	return file, nil
}
