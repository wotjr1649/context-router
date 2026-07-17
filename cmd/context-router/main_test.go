package main

import (
	"strings"
	"testing"
)

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    serverFlags
		wantErr bool
	}{
		{"defaults", nil, serverFlags{Profile: []string{"search", "fetch", "transform"}, LogLevel: "info"}, false},
		{"enable", []string{"--enable", "ingest,net"}, serverFlags{Profile: []string{"search", "fetch", "transform"}, Enable: []string{"ingest", "net"}, LogLevel: "info"}, false},
		{"unknown", []string{"--bogus"}, serverFlags{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFlags(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && strings.Join(got.Profile, ",") != strings.Join(tt.want.Profile, ",") {
				t.Fatalf("profile=%v want %v", got.Profile, tt.want.Profile)
			}
			if err == nil && strings.Join(got.Enable, ",") != strings.Join(tt.want.Enable, ",") {
				t.Fatalf("enable=%v want %v", got.Enable, tt.want.Enable)
			}
		})
	}
}

func TestBanner(t *testing.T) {
	f := serverFlags{Profile: []string{"search", "fetch", "transform"}, LogLevel: "info"}
	got := banner(f, "C:/proj")
	want := "[ctr] v" + version + " profile=search,fetch,transform ingest=off net=off root=C:/proj"
	if got != want {
		t.Fatalf("banner=%q want %q", got, want)
	}
	f2 := serverFlags{Profile: []string{"search"}, Enable: []string{"ingest"}, LogLevel: "info"}
	got2 := banner(f2, "/p")
	want2 := "[ctr] v" + version + " profile=search ingest=on net=off root=/p"
	if got2 != want2 {
		t.Fatalf("banner on-branch=%q want %q", got2, want2)
	}
}
