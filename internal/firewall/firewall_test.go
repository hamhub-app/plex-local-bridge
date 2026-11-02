package firewall

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

type fakeTables struct {
	tables map[string]map[string][][]string
	calls  [][]string
	fail   func(string, []string) bool
}

func newFake() *fakeTables {
	return &fakeTables{tables: map[string]map[string][][]string{
		"iptables":  {"PREROUTING": {{"-m", "comment", "--comment", "DSM", "-j", "RETURN"}}},
		"ip6tables": {"PREROUTING": {}},
	}}
}
func (f *fakeTables) run(_ context.Context, binary string, args ...string) ([]byte, error) {
	if !slices.Equal(args[:4], []string{"-w", "5", "-t", "nat"}) {
		return nil, errors.New("unexpected table")
	}
	a := args[4:]
	f.calls = append(f.calls, append([]string{binary}, a...))
	if f.fail != nil && f.fail(binary, a) {
		return nil, errors.New("injected failure")
	}
	table := f.tables[binary]
	if len(a) == 1 && a[0] == "-S" {
		return []byte("table exists"), nil
	}
	chain := a[1]
	rules, exists := table[chain]
	index := func(rule []string) int {
		for i, r := range rules {
			if slices.Equal(r, rule) {
				return i
			}
		}
		return -1
	}
	switch a[0] {
	case "-S":
		if !exists {
			return nil, errors.New("missing chain")
		}
		out := "-N " + chain + "\n"
		for _, r := range rules {
			out += "-A " + chain + " " + strings.Join(r, " ") + "\n"
		}
		return []byte(out), nil
	case "-N":
		if exists {
			return nil, errors.New("exists")
		}
		table[chain] = nil
	case "-C":
		if !exists || index(a[2:]) < 0 {
			return nil, errors.New("missing rule")
		}
	case "-A":
		if !exists {
			return nil, errors.New("missing chain")
		}
		table[chain] = append(rules, slices.Clone(a[2:]))
	case "-I":
		table[chain] = append([][]string{slices.Clone(a[3:])}, rules...)
	case "-D":
		i := index(a[2:])
		if i < 0 {
			return nil, errors.New("missing rule")
		}
		table[chain] = append(rules[:i], rules[i+1:]...)
	case "-F":
		table[chain] = nil
	case "-X":
		if len(rules) > 0 {
			return nil, errors.New("not empty")
		}
		delete(table, chain)
	default:
		return nil, fmt.Errorf("unexpected command %v", a)
	}
	return nil, nil
}
func manager(f *fakeTables) Manager {
	return Manager{Run: f.run, Binaries: []string{"iptables", "ip6tables"}, PublicPort: 32400, ListenPort: 13240}
}

func TestEnsureIdempotentAndRemoveOnlyOwnedRules(t *testing.T) {
	f := newFake()
	m := manager(f)
	ctx := context.Background()
	if err := m.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	count := len(f.calls)
	if err := m.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls[count:] {
		if slices.Contains([]string{"-F", "-A", "-I", "-N"}, call[1]) {
			t.Fatalf("repeated ensure mutated rules: %v", call)
		}
	}
	if len(f.tables["iptables"]["PREROUTING"]) != 2 {
		t.Fatal("lost original DSM rule")
	}
	if err := m.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx); err != nil {
		t.Fatal("cleanup must be idempotent:", err)
	}
	if len(f.tables["iptables"]["PREROUTING"]) != 1 || f.tables["iptables"]["PREROUTING"][0][3] != "DSM" {
		t.Fatal("modified DSM rule")
	}
	for _, call := range f.calls {
		if slices.Contains(call, "OUTPUT") || slices.Contains(call, "FORWARD") {
			t.Fatal("modified outbound/routed traffic:", call)
		}
		if (call[1] == "-F" || call[1] == "-X") && call[2] != Chain {
			t.Fatal("global/unowned cleanup:", call)
		}
	}
}

func TestPartialInstallCanBeRolledBack(t *testing.T) {
	f := newFake()
	m := manager(f)
	ctx := context.Background()
	f.fail = func(b string, a []string) bool { return b == "ip6tables" && a[0] == "-N" }
	if err := m.Ensure(ctx); err == nil {
		t.Fatal("expected IPv6 failure")
	}
	f.fail = nil
	if err := m.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.tables["iptables"]["PREROUTING"]) != 1 {
		t.Fatal("IPv4 interception remains after partial failure")
	}
}

func TestRefusesForeignChain(t *testing.T) {
	f := newFake()
	f.tables["iptables"][Chain] = [][]string{{"-j", "ACCEPT"}}
	m := manager(f)
	if err := m.Ensure(context.Background()); err == nil {
		t.Fatal("overwrote foreign chain")
	}
	if err := m.Remove(context.Background()); err == nil {
		t.Fatal("removed foreign chain")
	}
	for _, call := range f.calls {
		if call[1] != "-S" && call[1] != "-C" && call[0] == "iptables" {
			t.Fatal("mutated foreign rules:", call)
		}
	}
}

func TestCleanupReportsTableReadFailure(t *testing.T) {
	f := newFake()
	f.fail = func(_ string, a []string) bool { return a[0] == "-S" }
	if err := manager(f).Remove(context.Background()); err == nil {
		t.Fatal("reported success without access to NAT table")
	}
}

func TestTableReadFailureIncludesKernelDiagnostic(t *testing.T) {
	m := Manager{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("iptables: Permission denied (you must be root)\n"), errors.New("exit status 3")
	}, Binaries: []string{"iptables"}}
	err := m.Remove(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Permission denied") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("lost kernel diagnostic: %v", err)
	}
}

func TestCommandDiagnosticIsBounded(t *testing.T) {
	err := commandFailure("iptables", []string{"-N", Chain}, []byte(strings.Repeat("x", 10000)), errors.New("failed"))
	if len(err.Error()) > 4300 {
		t.Fatal("unbounded command diagnostic")
	}
}

func TestCleanupDetachesOwnJumpButPreservesMixedChain(t *testing.T) {
	f := newFake()
	m := manager(f)
	ctx := context.Background()
	if err := m.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	f.tables["iptables"][Chain] = append(f.tables["iptables"][Chain], []string{"-j", "RETURN"})
	if err := m.Remove(ctx); err == nil {
		t.Fatal("mixed chain must be reported")
	}
	if len(f.tables["iptables"]["PREROUTING"]) != 1 {
		t.Fatal("owned jump still intercepts original Plex port")
	}
	if len(f.tables["iptables"][Chain]) != 2 {
		t.Fatal("modified mixed chain")
	}
}

func TestRulesDoNotRequireCommentModule(t *testing.T) {
	f := newFake()
	m := manager(f)
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if slices.Contains(call, "--comment") {
			t.Fatalf("requires unavailable xt_comment: %v", call)
		}
	}
	for _, test := range []struct {
		rule  string
		valid bool
	}{
		{"-A " + Chain + " -p tcp -m tcp --dport 32400 -m addrtype --dst-type LOCAL -j REDIRECT --to-ports 13240", true},
		{"-A " + Chain + " -p tcp --dport 32400 -m addrtype --dst-type LOCAL -m comment --comment PlexLocalBridge -j REDIRECT --to-ports 13240", true},
		{"-A " + Chain + " -p tcp --dport 32400 -j REDIRECT --to-ports 13240", false},
		{"-A " + Chain + " -p tcp --dport 32400 -m addrtype --dst-type LOCAL -j ACCEPT", false},
	} {
		if got := ownedRedirect(strings.Fields(test.rule)); got != test.valid {
			t.Fatalf("ownedRedirect(%q)=%v", test.rule, got)
		}
	}
}
