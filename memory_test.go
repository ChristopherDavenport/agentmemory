package agentmemory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	tests := map[string]bool{
		"a": true, "style": true, "a-b-c": true, "a1-2b": true, "0": true,
		strings.Repeat("a", MaxNameBytes):   true,
		"":                                  false,
		"A":                                 false,
		"a b":                               false,
		"a_b":                               false,
		"a.b":                               false,
		"a--b":                              false,
		"-a":                                false,
		"a-":                                false,
		"ä":                                 false,
		"a/b":                               false,
		"..":                                false,
		strings.Repeat("a", MaxNameBytes+1): false,
	}
	for s, want := range tests {
		if got := ValidName(s); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestHash(t *testing.T) {
	if got := Hash(""); got != "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("Hash(\"\") = %s", got)
	}
	if Hash("a") == Hash("b") || !strings.HasPrefix(Hash("a"), "sha256:") || len(Hash("a")) != 7+64 {
		t.Error("Hash is not a sha256 digest")
	}
}

func TestCheckEntrySize(t *testing.T) {
	e := Entry{Scope: "user", Name: "a", Content: strings.Repeat("x", 10)}
	if err := CheckEntry(e, 0, nil); err != nil {
		t.Errorf("no limit: %v", err)
	}
	if err := CheckEntry(e, 10, nil); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	err := CheckEntry(e, 9, nil)
	var se *SizeError
	if !errors.As(err, &se) || se.Stored != -1 || !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if err.Error() != "agentmemory: entry user/a is 10 bytes, over the 9 byte limit; split it or trim it" {
		t.Errorf("message = %q", err)
	}
	err = CheckEntry(e, 9, &Entry{Content: "abc"})
	if err.Error() != "agentmemory: entry user/a is 10 bytes, over the 9 byte limit; it holds 3 bytes now; split it or trim it" {
		t.Errorf("message with stored = %q", err)
	}
}

func TestPutOptionsCheck(t *testing.T) {
	stored := &Entry{Scope: "user", Name: "a", Hash: Hash("x")}
	tests := []struct {
		name   string
		opts   []PutOption
		stored *Entry
		want   string // "" for ok, else a substring of the error
	}{
		{"unconditional, none", nil, nil, ""},
		{"unconditional, stored", nil, stored, ""},
		{"must not exist, none", []PutOption{IfHash("")}, nil, ""},
		{"must not exist, stored", []PutOption{IfHash("")}, stored, "already exists with hash " + Hash("x")},
		{"right hash", []PutOption{IfHash(Hash("x"))}, stored, ""},
		{"wrong hash", []PutOption{IfHash(Hash("y"))}, stored, "has hash " + Hash("x") + ", not " + Hash("y") + "; read it again"},
		{"expected, none", []PutOption{IfHash(Hash("x"))}, nil, "no longer exists; expected hash " + Hash("x")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ResolvePutOptions(tt.opts...).Check("user", "a", tt.stored)
			if tt.want == "" {
				if err != nil {
					t.Errorf("Check = %v", err)
				}
				return
			}
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Check = %v, want ErrConflict containing %q", err, tt.want)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	e := Entry{Name: "editor-choice", Content: "Uses Neovim.\nDark theme.", Meta: map[string]string{"description": "Editor preference"}}
	tests := map[string]bool{
		"":                true,
		"neovim":          true,
		"NEOVIM dark":     true,
		"editor":          true, // the name
		"preference":      true, // a meta value
		"choice":          true,
		"neovim light":    false,
		"emacs":           false,
		"  neovim  theme": true,
	}
	for q, want := range tests {
		if got := Match(e, q); got != want {
			t.Errorf("Match(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestSession(t *testing.T) {
	ctx := context.Background()
	if SessionFrom(ctx) != "" {
		t.Error("a bare context has a session")
	}
	if got := SessionFrom(WithSession(ctx, "s1")); got != "s1" {
		t.Errorf("SessionFrom = %q", got)
	}
}
