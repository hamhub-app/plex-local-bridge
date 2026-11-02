package firewall

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestModuleDependenciesLoadedBeforeTargets(t *testing.T) {
	deps := map[string][]string{"iptable_nat": {"ip_tables", "nf_nat"}, "ip_tables": {"x_tables"}, "nf_nat": {"nf_conntrack"}, "xt_REDIRECT": {"nf_nat"}}
	loaded := map[string]bool{"nf_conntrack": true}
	var order []string
	l := ModuleLoader{Loaded: func(n string) bool { return loaded[n] }, Dependencies: func(n string) ([]string, error) { return deps[n], nil }, Insert: func(_ context.Context, n string) error { order = append(order, n); loaded[n] = true; return nil }}
	if err := l.Load(context.Background(), "iptable_nat", "xt_REDIRECT", "iptable_nat"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"x_tables", "ip_tables", "nf_nat", "iptable_nat", "xt_REDIRECT"}) {
		t.Fatal(order)
	}
}

func TestModuleDependencyFailuresNeverInsertTarget(t *testing.T) {
	for _, unsafe := range []string{"../outside", "a/b", "x;bad", "cycle", "missing"} {
		var inserted bool
		l := ModuleLoader{Loaded: func(string) bool { return false }, Dependencies: func(n string) ([]string, error) {
			if n == "missing" {
				return nil, errors.New("missing module")
			}
			return []string{n}, nil
		}, Insert: func(context.Context, string) error { inserted = true; return nil }}
		if err := l.Load(context.Background(), unsafe); err == nil || inserted {
			t.Fatalf("unsafe module %q inserted", unsafe)
		}
	}
}

func TestModuleMetadataRequiresExactKernel(t *testing.T) {
	data := []byte("license=GPL\x00depends=nf_nat,ip_tables\x00vermagic=4.4.302+ SMP mod_unload\x00")
	deps, err := moduleMetadata(data, "4.4.302+")
	if err != nil || !slices.Equal(deps, []string{"nf_nat", "ip_tables"}) {
		t.Fatalf("%v %v", deps, err)
	}
	if _, err := moduleMetadata(data, "4.4.302"); err == nil {
		t.Fatal("accepted different kernel")
	}
	if _, err := moduleMetadata([]byte("depends=nf_nat\x00"), "4.4.302+"); err == nil {
		t.Fatal("accepted module without kernel metadata")
	}
}
