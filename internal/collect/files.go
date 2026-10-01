package collect

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Fingerprint is what the platform is told of one configuration file: where
// it is, the SHA-256 of what it held, and its size. **Never its content**:
// configuration holds passwords and keys, and "this file changed at this
// time" is all the platform needs to line it up with an incident.
type Fingerprint struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// The most files one report holds, and the largest file read (the platform
// takes 200; a file over the limit is a log or a dump, not configuration).
const (
	FilesMax    = 200
	FileMaxSize = 8 << 20
)

// Fingerprints reads the files the machine's owner listed ("files" in the
// agent's file), and only those. A pattern is an absolute path, and may have
// a wildcard in its file name (`/etc/php/8.3/fpm/pool.d/*.conf`); it may not
// go up a directory. A file that is not there is simply not in the answer,
// so the platform sees it removed; a file that is there and cannot be read
// makes the whole report fail, because leaving it out would read as removed.
//
// root is where the host's filesystem is when the agent runs in a container;
// the paths reported are the host's.
func Fingerprints(root string, patterns []string) ([]Fingerprint, error) {
	seen := map[string]bool{}
	var found []Fingerprint

	for _, pattern := range patterns {
		clean, err := cleanPattern(pattern)
		if err != nil {
			return nil, err
		}

		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(clean)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pattern, err)
		}

		for _, match := range matches {
			reported := reportedPath(root, match)

			if seen[reported] {
				continue
			}

			info, err := os.Stat(match)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			if err != nil {
				return nil, err
			}

			if !info.Mode().IsRegular() || info.Size() > FileMaxSize {
				continue
			}

			sum, size, err := hashFile(match)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			if err != nil {
				return nil, err
			}

			seen[reported] = true
			found = append(found, Fingerprint{Path: reported, SHA256: sum, Size: size})
		}
	}

	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })

	if len(found) > FilesMax {
		found = found[:FilesMax]
	}

	return found, nil
}

// cleanPattern is a listed path, checked: absolute, and nowhere above itself.
func cleanPattern(pattern string) (string, error) {
	pattern = strings.TrimSpace(pattern)

	if !strings.HasPrefix(pattern, "/") {
		return "", fmt.Errorf("%q: a file to fingerprint is an absolute path", pattern)
	}

	for _, part := range strings.Split(pattern, "/") {
		if part == ".." {
			return "", fmt.Errorf("%q: a path may not go up a directory", pattern)
		}
	}

	return path.Clean(pattern), nil
}

func reportedPath(root, match string) string {
	relative, err := filepath.Rel(root, match)
	if err != nil {
		return filepath.ToSlash(match)
	}

	return "/" + filepath.ToSlash(relative)
}

func hashFile(name string) (string, int64, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}

	defer file.Close()

	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, FileMaxSize+1))

	if err != nil {
		return "", 0, err
	}

	if size > FileMaxSize {
		return "", 0, fmt.Errorf("%s grew past %d bytes while it was read", name, FileMaxSize)
	}

	return hex.EncodeToString(hash.Sum(nil)), size, nil
}
