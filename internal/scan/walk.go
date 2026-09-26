package scan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Walk lists the files to process under abs in sorted order: the path itself
// when it is not a directory, otherwise every non-directory entry below it
// except .DS_Store and names starting with .amuxify-. Symlinks are listed
// (ScanFile reports them) and never followed, so a symlink somewhere in the
// tree cannot abort the walk (guarantee 3).
//
// A directory that cannot be read, or an entry that vanishes or errors while
// the tree is listed, does not stop the walk either: every readable entry is
// still listed, and the returned error names each unreadable one. Callers
// process the paths they got and report that error at run level, so a tree
// with an unreadable corner is never reported as PASS with the corner missing
// (review C2).
func Walk(abs string) ([]string, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []string{abs}, nil
	}
	var paths []string
	var unreadable []string
	walkErr := filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("cannot read %s: %v", p, err))
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
	if len(unreadable) > 0 {
		return paths, errors.New(strings.Join(unreadable, "; "))
	}
	return paths, nil
}
