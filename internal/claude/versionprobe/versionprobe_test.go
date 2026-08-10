package versionprobe

import "testing"

func TestCompatible(t *testing.T) {
	v := Version{2, 1, 226}
	for _, tc := range []struct {
		constraint string
		want       bool
	}{{"", true}, {"2.1.226", true}, {">=2.1.0 <2.2.0", true}, {">=2.2.0", false}, {"<2.1.226", false}} {
		got, err := Compatible(v, tc.constraint)
		if err != nil || got != tc.want {
			t.Fatalf("Compatible(%q) = %v, %v; want %v", tc.constraint, got, err, tc.want)
		}
	}
	if _, err := Compatible(v, ">=broken"); err == nil {
		t.Fatal("invalid constraint accepted")
	}
}
