package migrate

import (
	"os"
	"strings"
	"testing"
)

// The Bash check PF13 and the controller must fingerprint the schema with
// the same query, or every deploy after the first would report drift.
func TestFingerprintSQLMatchesPreflight(t *testing.T) {
	b, err := os.ReadFile("../../preflight/sql/schema_fingerprint.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSuffix(strings.TrimSpace(string(b)), ";") != strings.TrimSpace(FingerprintSQL) {
		t.Fatal("preflight/sql/schema_fingerprint.sql differs from migrate.FingerprintSQL")
	}
}
