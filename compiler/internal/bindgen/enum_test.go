package bindgen

import (
	"reflect"
	"strings"
	"testing"
)

// #720: an enum value is any string, and its variant's name is derived from
// it (idlEnumValueToPascal), so two values can generate one variant. A
// duplicate variant would not compile: each value after the first is a bind
// error at the enum's position, reported with every other failure.
func TestWebIdlEnumVariantCollision(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string // the error's lines, in order
	}{
		{
			name: "separators differ",
			src:  `enum E { "a-b", "a_b" };`,
			want: []string{`dict.webidl:1:1: enum E: values "a-b" and "a_b" both generate variant AB`},
		},
		{
			name: "empty and Empty",
			src:  `enum E { "", "empty" };`,
			want: []string{`dict.webidl:1:1: enum E: values "" and "empty" both generate variant Empty`},
		},
		{
			// Each later value is reported against the first, not the one
			// before it.
			name: "three values on one variant",
			src:  `enum E { "x-y", "x_y", "x y" };`,
			want: []string{
				`dict.webidl:1:1: enum E: values "x-y" and "x_y" both generate variant XY`,
				`dict.webidl:1:1: enum E: values "x-y" and "x y" both generate variant XY`,
			},
		},
		{
			// WebIDL forbids a repeated value, but the parser keeps it.
			name: "repeated value",
			src:  `enum E { "a", "a" };`,
			want: []string{`dict.webidl:1:1: enum E: values "a" and "a" both generate variant A`},
		},
		{
			// A collision is within one enum: A's "a-b" and B's "a_b" both
			// generate AB, in different types.
			name: "values of different enums",
			src: `enum A { "a-b" };
enum B { "a_b", "a b" };`,
			want: []string{`dict.webidl:2:1: enum B: values "a_b" and "a b" both generate variant AB`},
		},
		{
			name: "reported with a broken dictionary",
			src: `dictionary D : Missing {};
enum E { "a-b", "a_b" };`,
			want: []string{
				`dict.webidl:2:1: enum E: values "a-b" and "a_b" both generate variant AB`,
				"dictionary D inherits from Missing, which is not a dictionary in the bound IDL files",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			modules, err := WebIdlToIR(parseMerged(t, tc.src))
			if err == nil {
				t.Fatalf("WebIdlToIR succeeded with %d modules, want the collision error", len(modules))
			}
			if got := strings.Split(err.Error(), "\n"); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("error:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Values that generate distinct variants convert, one case per value, in order.
func TestWebIdlEnumVariantNames(t *testing.T) {
	modules, err := WebIdlToIR(parseMerged(t, `enum E { "2d", "text/html", "" };`))
	if err != nil {
		t.Fatalf("WebIdlToIR: %v", err)
	}
	var got []string
	for _, c := range modules[0].Types[0].Cases {
		got = append(got, c.Name)
	}
	if want := []string{"V2d", "TextHtml", "Empty"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cases = %q, want %q", got, want)
	}
}
