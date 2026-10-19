package internal

import (
	"reflect"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: "migrate", want: []string{"migrate"}},
		{in: "  npm run migrate  ", want: []string{"npm", "run", "migrate"}},
		{in: `sh -c "npm run migrate && echo done"`, want: []string{"sh", "-c", "npm run migrate && echo done"}},
		{in: `echo 'it''s' "a \"quoted\" word"`, want: []string{"echo", "its", `a "quoted" word`}},
		{in: `printf 'a\nb'`, want: []string{"printf", `a\nb`}},                       // single quotes are literal
		{in: `echo "C:\\path" "keep\n"`, want: []string{"echo", `C:\path`, `keep\n`}}, // only \" and \\ escape inside double quotes
		{in: `echo escaped\ space`, want: []string{"echo", "escaped space"}},
		{in: `cmd ""`, want: []string{"cmd", ""}},
		{in: `a	b
c`, want: []string{"a", "b", "c"}},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: `echo "unterminated`, wantErr: true},
		{in: `echo 'unterminated`, wantErr: true},
		{in: `echo trailing\`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := SplitCommand(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SplitCommand(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SplitCommand(%q) unexpected error: %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitCommand(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// What the plan prints must split back to the same arguments, so an operator
// can copy it into a shell and get the same command.
func TestQuoteCommandRoundTrip(t *testing.T) {
	for _, args := range [][]string{
		{"migrate"},
		{"sh", "-c", "npm run migrate && echo done"},
		{"echo", `a "quoted" word`, "it's", ""},
	} {
		got, err := SplitCommand(quoteCommand(args))
		if err != nil {
			t.Fatalf("quoteCommand(%q) = %q does not split: %v", args, quoteCommand(args), err)
		}
		if !reflect.DeepEqual(got, args) {
			t.Errorf("round trip of %q via %q = %q", args, quoteCommand(args), got)
		}
	}
}
