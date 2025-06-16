package semver

import "testing"

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"1.2.3", "1.2.3", 0}, {"1.2.10", "1.2.9", 1}, {"1.10.0", "1.9.9", 1}, {"0.9.0", "1.0.0", -1}, {"v2.0.0", "1.99.99", 1}} {
		got, err := Compare(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("Compare(%s,%s) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
	}
	for _, bad := range []string{"1.2", "1.2.x", "01.2.3", ""} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}
