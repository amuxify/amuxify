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
// except .DS_Store and the temp files amuxify writes, whose names start with
// .amuxify- and end with .tmp (fsutil.TempName); any other name is listed,
// so a planted ".amuxify-evil.exe" is scanned like every other file.
// Symlinks are listed (ScanFile reports them) and never followed, so a
// symlink somewhere in the tree cannot abort the walk (guarantee 3).
//
// Each path in exclude names a directory that is not entered; the scanner
// passes its quarantine directory so a quarantine that sits inside the tree
// is not walked and its files are not quarantined again one level deeper.
// An excluded directory that exists is recognised by identity (os.SameFile
// on its device and inode), so the spelling the walk meets may differ from
// the one given through a symlink, a ".." component, a relative path or
// different letter case on a case-insensitive filesystem; one that does not
// exist yet is matched by its cleaned absolute path.
//
// A directory that cannot be read, or an entry that vanishes or errors while
// the tree is listed, does not stop the walk either: every readable entry is
// still listed, and the returned error names each unreadable one. Callers
// process the paths they got and report that error at run level, so a tree
// with an unreadable corner is never reported as PASS with the corner missing
// (review C2).
func Walk(abs string, exclude ...string) ([]string, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []string{abs}, nil
	}
	skip := map[string]bool{}
	var skipInfo []os.FileInfo
	for _, e := range exclude {
		if a, err := filepath.Abs(e); err == nil {
			e = a
		}
		skip[filepath.Clean(e)] = true
		if fi, err := os.Stat(e); err == nil && fi.IsDir() {
			skipInfo = append(skipInfo, fi)
		}
	}
	var paths []string
	var unreadable []string
	walkErr := filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("cannot read %s: %v", p, err))
			return nil
		}
		if d.IsDir() {
			if skip[filepath.Clean(p)] {
				return filepath.SkipDir
			}
			if len(skipInfo) > 0 {
				if fi, err := d.Info(); err == nil {
					for _, x := range skipInfo {
						if os.SameFile(fi, x) {
							return filepath.SkipDir
						}
					}
				}
			}
			return nil
		}
		if d.Name() == ".DS_Store" || isTempName(d.Name()) {
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

// isTempName reports whether name has the shape fsutil.TempName produces,
// ".amuxify-<name>.tmp". Only that shape is a temp file of amuxify's own.
func isTempName(name string) bool {
	return strings.HasPrefix(name, ".amuxify-") && strings.HasSuffix(name, ".tmp")
}
