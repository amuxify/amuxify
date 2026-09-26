package scan

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Walk lists the files to process under abs in sorted order: the path itself
// when it is not a directory, otherwise every non-directory entry below it
// except .DS_Store and names starting with .amuxify-. Unreadable entries are
// skipped. Symlinks are listed (ScanFile reports them).
func Walk(abs string) ([]string, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []string{abs}, nil
	}
	var paths []string
	walkErr := filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == ".DS_Store" || strings.HasPrefix(d.Name(), ".amuxify-") {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(paths)
	return paths, nil
}
