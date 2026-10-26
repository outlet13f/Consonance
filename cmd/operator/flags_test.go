package main

import "testing"

func TestLeaderNamespace(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"election off", nil, ""},
		{"election off, namespace given", []string{"--leader-election-namespace", "other"}, ""},
		{"election on, default namespace", []string{"--leader-elect"}, "consonance-system"},
		{"election on, namespace given", []string{"--leader-elect", "--leader-election-namespace", "other"}, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseFlags(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got := leaderNamespace(c); got != tc.want {
				t.Errorf("leaderNamespace = %q, want %q", got, tc.want)
			}
		})
	}
}
