package ai

import "testing"

// CleanJSON is pure and central to parsing every LLM response, so it gets one test.
func TestCleanJSON(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"fenced array":                {"```json\n[{\"a\":1}]\n```", `[{"a":1}]`},
		"object w/ prose":             {"Sure! Here you go:\n{\"x\":2}\nHope that helps.", `{"x":2}`},
		"plain array":                 {`[1,2,3]`, `[1,2,3]`},
		"object before array in text": {`prefix {"k":[1,2]} suffix`, `{"k":[1,2]}`},
	}
	for name, c := range cases {
		if got := CleanJSON(c.in); got != c.want {
			t.Errorf("%s: CleanJSON(%q) = %q, want %q", name, c.in, got, c.want)
		}
	}
}
