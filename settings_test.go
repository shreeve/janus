package janus

import "testing"

func TestResolveBool(t *testing.T) {
	on, off := true, false
	tests := []struct {
		name    string
		site    *bool
		global  *bool
		builtin bool
		want    bool
	}{
		{"all unset → builtin off", nil, nil, false, false},
		{"all unset → builtin on", nil, nil, true, true},
		{"global on, site unset", nil, &on, false, true},
		{"global off, site unset", nil, &off, true, false},
		{"site off overrides global on", &off, &on, false, false},
		{"site on overrides global off", &on, &off, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBool(tt.site, tt.global, tt.builtin); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseOnOff(t *testing.T) {
	tests := []struct {
		args    []string
		want    bool
		wantErr bool
	}{
		{nil, true, false},
		{[]string{}, true, false},
		{[]string{"on"}, true, false},
		{[]string{"off"}, false, false},
		{[]string{"maybe"}, false, true},
		{[]string{"on", "off"}, false, true},
	}
	for _, tt := range tests {
		got, err := parseOnOff(tt.args)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("args %v: want error", tt.args)
			}
			continue
		}
		if err != nil {
			t.Fatalf("args %v: %v", tt.args, err)
		}
		if got != tt.want {
			t.Fatalf("args %v: got %v, want %v", tt.args, got, tt.want)
		}
	}
}
