package parser

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in       string
		wantName string
		wantArgs []string
	}{
		{"SET name raiyaan", "SET", []string{"name", "raiyaan"}},
		{"set name raiyaan", "SET", []string{"name", "raiyaan"}},
		{"  GET   name  ", "GET", []string{"name"}},
		{`SET greeting "hello world"`, "SET", []string{"greeting", "hello world"}},
		{`SET q "say \"hi\""`, "SET", []string{"q", `say "hi"`}},
		{`SET empty ""`, "SET", []string{"empty", ""}},
		{"KEYS", "KEYS", []string{}},
		{"", "", nil},
		{"   ", "", nil},
	}
	for _, tt := range tests {
		cmd, err := Parse(tt.in)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", tt.in, err)
		}
		if cmd.Name != tt.wantName {
			t.Errorf("Parse(%q).Name = %q, want %q", tt.in, cmd.Name, tt.wantName)
		}
		if len(cmd.Args) == 0 && len(tt.wantArgs) == 0 {
			continue
		}
		if !reflect.DeepEqual(cmd.Args, tt.wantArgs) {
			t.Errorf("Parse(%q).Args = %q, want %q", tt.in, cmd.Args, tt.wantArgs)
		}
	}
}

func TestUnclosedQuote(t *testing.T) {
	if _, err := Parse(`SET k "oops`); err != ErrUnclosedQuote {
		t.Fatalf("err = %v, want ErrUnclosedQuote", err)
	}
}
