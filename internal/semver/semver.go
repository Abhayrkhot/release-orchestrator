// Package semver compares MAJOR.MINOR.PATCH versions. Pre-release and build
// suffixes are not supported: release versions here are plain triples.
package semver

import (
	"fmt"
	"strconv"
	"strings"
)

type Version struct{ Major, Minor, Patch int }

func Parse(s string) (Version, error) {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q: want MAJOR.MINOR.PATCH", s)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("version %q: bad component %q", s, p)
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2]}, nil
}

// Compare returns -1, 0 or 1. Both versions must parse.
func Compare(a, b string) (int, error) {
	va, err := Parse(a)
	if err != nil {
		return 0, err
	}
	vb, err := Parse(b)
	if err != nil {
		return 0, err
	}
	for _, d := range [][2]int{{va.Major, vb.Major}, {va.Minor, vb.Minor}, {va.Patch, vb.Patch}} {
		if d[0] < d[1] {
			return -1, nil
		}
		if d[0] > d[1] {
			return 1, nil
		}
	}
	return 0, nil
}
