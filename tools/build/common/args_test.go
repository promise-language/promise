package common

import (
	"testing"

	"slices"
)

func TestNormalizeArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, []string{}},
		{"no flags", []string{"go", "promise", "all"}, []string{"go", "promise", "all"}},
		{"single dash preserved", []string{"-flag"}, []string{"-flag"}},
		{"double dash to single", []string{"--flag"}, []string{"-flag"}},
		{"equals split", []string{"-flag=value"}, []string{"-flag", "value"}},
		{"colon split", []string{"-flag:value"}, []string{"-flag", "value"}},
		{"double dash equals", []string{"--flag=value"}, []string{"-flag", "value"}},
		{"double dash colon", []string{"--flag:value"}, []string{"-flag", "value"}},
		{"first equals only", []string{"-o=dir/file=v2.txt"}, []string{"-o", "dir/file=v2.txt"}},
		{"first colon only", []string{"-o:C:\\path"}, []string{"-o", "C:\\path"}},
		{"bare dash unchanged", []string{"-"}, []string{"-"}},
		{"bare double dash unchanged", []string{"--"}, []string{"--"}},
		{"mixed args", []string{"--wasm", "go", "-shared", "--timeout=5s"}, []string{"-wasm", "go", "-shared", "-timeout", "5s"}},
		{"idempotent", []string{"-flag", "value"}, []string{"-flag", "value"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeArgs(tt.in)
			if tt.in == nil {
				if got != nil {
					t.Fatalf("NormalizeArgs(nil) = %v, want nil", got)
				}
				return
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("NormalizeArgs(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
