package types

import (
	"encoding/json"
	"slices"
	"testing"
)

// A caller passing a single git remote should not have to remember brackets.
func TestStringListAcceptsBareString(t *testing.T) {
	var body struct {
		Repos StringList `json:"repos"`
	}
	if err := json.Unmarshal([]byte(`{"repos":"github.com/org/repo"}`), &body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(body.Repos, []string{"github.com/org/repo"}) {
		t.Fatalf("got %v", body.Repos)
	}
}

func TestStringListAcceptsArray(t *testing.T) {
	var body struct {
		Repos StringList `json:"repos"`
	}
	if err := json.Unmarshal([]byte(`{"repos":["a","b"]}`), &body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(body.Repos, []string{"a", "b"}) {
		t.Fatalf("got %v", body.Repos)
	}
}

// Omitting the field must stay distinguishable from clearing it, since the
// write handler uses nil to mean "leave unchanged".
func TestStringListOmittedStaysNil(t *testing.T) {
	var body struct {
		Repos StringList `json:"repos"`
	}
	if err := json.Unmarshal([]byte(`{}`), &body); err != nil {
		t.Fatal(err)
	}
	if body.Repos != nil {
		t.Fatalf("got %v, want nil", body.Repos)
	}
}

func TestStringListRejectsWrongType(t *testing.T) {
	var body struct {
		Repos StringList `json:"repos"`
	}
	if err := json.Unmarshal([]byte(`{"repos":42}`), &body); err == nil {
		t.Fatal("want an error for a numeric value")
	}
}
