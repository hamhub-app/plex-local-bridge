package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, test := range []struct {
		name, upstream      string
		public, listen, max int
		valid               bool
	}{
		{"auto", "auto", 32400, 13240, 256, true},
		{"local", "http://127.0.0.1:32400", 32400, 13240, 1, true},
		{"loop", "http://127.0.0.1:13240", 32400, 13240, 1, false},
		{"remote", "http://203.0.113.9:32400", 32400, 13240, 1, false},
		{"dns", "http://localhost:32400", 32400, 13240, 1, false},
		{"credentials", "http://token@127.0.0.1:32400", 32400, 13240, 1, false},
		{"query", "http://127.0.0.1:32400/?token=secret", 32400, 13240, 1, false},
		{"path", "http://127.0.0.1:32400/media", 32400, 13240, 1, false},
		{"https", "https://127.0.0.1:32400", 32400, 13240, 1, false},
		{"missing port", "http://127.0.0.1", 32400, 13240, 1, false},
		{"privileged port", "auto", 80, 13240, 1, false},
		{"same ports", "auto", 32400, 32400, 1, false},
		{"auto upstream loop", "auto", 32401, 32400, 1, false},
		{"unbounded", "auto", 32400, 13240, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Config{UpstreamURL: test.upstream, PublicPort: test.public, ListenPort: test.listen, MaxConnections: test.max}
			if err := c.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate()=%v, valid=%v", err, test.valid)
			}
		})
	}
}

func TestLoadAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Default()); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got != Default() {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, data := range []string{`null`, `[]`, `{} {}`, `{"unknown":1}`, `{"listen_port":32400}`, `{}`, strings.Repeat(" ", 65537)} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if (err == nil) != (data == "{}") {
			t.Errorf("unexpected result for %q: %v", data[:min(len(data), 50)], err)
		}
	}
}

func TestDefaultInterface(t *testing.T) {
	routes := "Iface Destination Gateway Flags RefCnt Use Metric Mask\n" +
		"docker0 000011AC 00000000 0001 0 0 0 0000FFFF\n" +
		"ovs_bond0 00000000 010032C0 0003 0 0 0 00000000\n"
	if got := defaultInterface(routes); got != "ovs_bond0" {
		t.Fatal(got)
	}
	if got := defaultInterface("eth0 00000000 0 0000 0 0 0 00000000"); got != "" {
		t.Fatal(got)
	}
}
