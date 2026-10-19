package daemon

import "testing"

// Upstream's sys/windows.rs test of libuv's argument quoting.
func TestQuotesArgumentsLikeLibuv(t *testing.T) {
	cases := []struct{ argument, want string }{
		{``, `""`},
		{`plain`, `plain`},
		{`a b`, `"a b"`},
		{`*.txt`, `*.txt`},
		{`say "hi"`, `"say \"hi\""`},
		{`C:\dir with space\`, `"C:\dir with space\\"`},
		{`a\\b c`, `"a\\b c"`},
	}
	for _, c := range cases {
		if got := quoteArgument(c.argument); got != c.want {
			t.Errorf("quoteArgument(%q) = %q, want %q", c.argument, got, c.want)
		}
	}
}
