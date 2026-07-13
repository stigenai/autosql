package start

import (
	"strings"
	"testing"
)

func TestSpecDigestAndValidation(t *testing.T) {
	s, err := New("release_two", strings.Repeat("a", 64), "v1", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.NewVersion = "v3"
	if err = s.Validate(); err == nil {
		t.Fatal("tampered spec accepted")
	}
}
