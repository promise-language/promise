package main

import "testing"

func TestSanitizeTempPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"promise-inst-Vector[int]", "promise-inst-Vector[int]"},
		{"promise-inst-Vector[int?]", "promise-inst-Vector[int_]"},
		{"promise-inst-Map[string, int*]", "promise-inst-Map[string, int_]"},
		{"promise-mod-foo:bar", "promise-mod-foo_bar"},
		{"promise-inst-Box[Vector[int?]]", "promise-inst-Box[Vector[int_]]"},
		{"a/b\\c<d>e|f\"g", "a_b_c_d_e_f_g"},
		{"clean-name", "clean-name"},
		{"with\x01control\x1f", "with_control_"},
	}
	for _, c := range cases {
		got := sanitizeTempPrefix(c.in)
		if got != c.want {
			t.Errorf("sanitizeTempPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
