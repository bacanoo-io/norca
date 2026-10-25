package storage

import (
	"bytes"
	"testing"
)

// prefixSuccessor is pure and DB-independent, so it is unit-tested without the
// integration tag. It must bound a prefix range: every key with the prefix is
// < the successor, and no shorter/other key sneaks in.
func TestPrefixSuccessor(t *testing.T) {
	cases := []struct {
		in   string
		want []byte // nil means "no upper bound"
	}{
		{"a/", []byte("a0")},   // '/' (0x2f) -> '0' (0x30)
		{"ab", []byte("ac")},   // last byte incremented
		{"a\xff", []byte("b")}, // carry: trailing 0xFF dropped, previous incremented
		{"\xff\xff", nil},      // all 0xFF -> unbounded
	}
	for _, c := range cases {
		got := prefixSuccessor([]byte(c.in))
		if !bytes.Equal(got, c.want) {
			t.Errorf("prefixSuccessor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPrefixSuccessorBoundsRange(t *testing.T) {
	prefix := []byte("secret/data/")
	hi := prefixSuccessor(prefix)
	// A key under the prefix sorts within [prefix, hi).
	under := []byte("secret/data/app")
	if bytes.Compare(under, prefix) < 0 || bytes.Compare(under, hi) >= 0 {
		t.Fatalf("key under prefix fell outside [prefix, successor)")
	}
	// A sibling that shares a proper prefix but not the full segment is excluded.
	sibling := []byte("secret/datax")
	if bytes.Compare(sibling, prefix) >= 0 && bytes.Compare(sibling, hi) < 0 {
		t.Fatalf("sibling %q wrongly included in [prefix, successor)", sibling)
	}
}

// The declared schema version must be exactly the number of migrations, or a
// database would be recorded at a version whose migration never ran.
func TestMySQLSchemaVersionMatchesMigrations(t *testing.T) {
	if len(mysqlMigrations) != mysqlSchemaVersion {
		t.Fatalf("mysqlSchemaVersion = %d but there are %d migrations", mysqlSchemaVersion, len(mysqlMigrations))
	}
}
