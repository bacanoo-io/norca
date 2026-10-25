package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func TestParseAndEvaluate(t *testing.T) {
	p, err := Parse("app", []byte(`{
		"path": {
			"secret/data/app/*": {"capabilities": ["read", "list"]},
			"secret/data/app/config": {"capabilities": ["read", "update"]}
		}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	acl := NewACL(p)

	cases := []struct {
		path string
		cap  Capability
		want bool
	}{
		{"secret/data/app/db", Read, true},
		{"secret/data/app/db", List, true},
		{"secret/data/app/db", Create, false}, // not granted
		{"secret/data/app/config", Update, true},
		{"secret/data/other", Read, false}, // no matching rule -> default deny
	}
	for _, c := range cases {
		if got := acl.Allows(c.path, c.cap); got != c.want {
			t.Errorf("Allows(%q,%s) = %v, want %v", c.path, c.cap, got, c.want)
		}
	}
}

func TestExactMatch(t *testing.T) {
	p, _ := Parse("x", []byte(`{"path": {"secret/data/one": {"capabilities": ["read"]}}}`))
	acl := NewACL(p)
	if !acl.Allows("secret/data/one", Read) {
		t.Fatal("exact path not allowed")
	}
	if acl.Allows("secret/data/one/two", Read) {
		t.Fatal("exact rule wrongly matched a sub-path")
	}
}

func TestDenyOverrides(t *testing.T) {
	p, _ := Parse("x", []byte(`{
		"path": {
			"secret/*": {"capabilities": ["read"]},
			"secret/data/admin/*": {"capabilities": ["deny"]}
		}
	}`))
	acl := NewACL(p)
	if !acl.Allows("secret/data/app/x", Read) {
		t.Fatal("read on secret/* should be allowed")
	}
	if acl.Allows("secret/data/admin/root", Read) {
		t.Fatal("deny on admin path must override the broad read grant")
	}
}

func TestMultiplePoliciesUnion(t *testing.T) {
	readOnly, _ := Parse("ro", []byte(`{"path": {"secret/data/a/*": {"capabilities": ["read"]}}}`))
	writer, _ := Parse("w", []byte(`{"path": {"secret/data/b/*": {"capabilities": ["create","update"]}}}`))
	acl := NewACL(readOnly, writer)

	if !acl.Allows("secret/data/a/x", Read) {
		t.Fatal("read from first policy denied")
	}
	if !acl.Allows("secret/data/b/x", Create) {
		t.Fatal("create from second policy denied")
	}
	if acl.Allows("secret/data/a/x", Create) {
		t.Fatal("create should not be granted on path a")
	}
}

func TestParseUnknownCapability(t *testing.T) {
	if _, err := Parse("x", []byte(`{"path": {"p": {"capabilities": ["frobnicate"]}}}`)); !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("want ErrUnknownCapability, got %v", err)
	}
}

func TestParseMalformed(t *testing.T) {
	if _, err := Parse("x", []byte(`{not json`)); !errors.Is(err, ErrMalformedPolicy) {
		t.Fatalf("want ErrMalformedPolicy, got %v", err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewStore(storage.NewMemoryBackend())

	p, _ := Parse("readers", []byte(`{"path": {"secret/data/app/*": {"capabilities": ["read","list"]}}}`))
	if err := s.Set(ctx, p); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := s.Get(ctx, "readers")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	acl := NewACL(got)
	if !acl.Allows("secret/data/app/db", Read) {
		t.Fatal("round-tripped policy does not grant read")
	}

	names, err := s.List(ctx)
	if err != nil || len(names) != 1 || names[0] != "readers" {
		t.Fatalf("List = %v, err %v", names, err)
	}

	if err := s.Delete(ctx, "readers"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "readers"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("after delete: want ErrPolicyNotFound, got %v", err)
	}
}

func TestStoreInvalidName(t *testing.T) {
	ctx := context.Background()
	s := NewStore(storage.NewMemoryBackend())
	for _, name := range []string{"", "has/slash", ".."} {
		if err := s.Set(ctx, &Policy{Name: name}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Set(%q): want ErrInvalidName, got %v", name, err)
		}
	}
}

func TestGetMissing(t *testing.T) {
	ctx := context.Background()
	s := NewStore(storage.NewMemoryBackend())
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("want ErrPolicyNotFound, got %v", err)
	}
}

func TestTemplatedExpandsAndDrops(t *testing.T) {
	resolve := func(key string) (string, bool) {
		switch key {
		case "identity.entity.id":
			return "abc123", true
		case "identity.entity.name":
			return "alice", true
		default:
			return "", false
		}
	}
	p := &Policy{Name: "t", Rules: []Rule{
		{Path: "secret/data/users/{{identity.entity.name}}/*", Capabilities: []Capability{Read}},
		{Path: "cubbyhole/{{ identity.entity.id }}", Capabilities: []Capability{Create}},
		{Path: "secret/data/team/{{identity.entity.metadata.team}}/*", Capabilities: []Capability{Read}}, // unresolved -> dropped
		{Path: "secret/data/shared", Capabilities: []Capability{Read}},                                   // no template -> kept
	}}
	out := p.Templated(resolve)

	got := map[string]bool{}
	for _, r := range out.Rules {
		got[r.Path] = true
	}
	if !got["secret/data/users/alice/*"] {
		t.Fatalf("name template not expanded: %v", got)
	}
	if !got["cubbyhole/abc123"] {
		t.Fatalf("id template (with spaces) not expanded: %v", got)
	}
	if !got["secret/data/shared"] {
		t.Fatalf("non-templated rule dropped: %v", got)
	}
	if len(out.Rules) != 3 {
		t.Fatalf("expected 3 rules (one dropped), got %d: %v", len(out.Rules), got)
	}
}

func TestTemplatedNoTemplatesReturnsSame(t *testing.T) {
	p := &Policy{Name: "t", Rules: []Rule{{Path: "secret/data/x", Capabilities: []Capability{Read}}}}
	if out := p.Templated(func(string) (string, bool) { return "", false }); out != p {
		t.Fatal("a policy with no templates should be returned unchanged (same pointer)")
	}
}
