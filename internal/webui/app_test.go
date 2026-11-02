package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hamhub-app/plex-local-bridge/internal/config"
)

func fixture(t *testing.T) (*App, *[]string) {
	t.Helper()
	dir := t.TempDir()
	calls := []string{}
	a := &App{Version: "test", ConfigPath: filepath.Join(dir, "config.json"), VarPath: dir, StatePath: filepath.Join(dir, "run"), GatewayPath: filepath.Join(dir, "gateway"),
		Authenticate: func(r *http.Request) (string, error) {
			if r.Header.Get("Cookie") != "id=fixture" {
				return "", apiError{401, "not logged in"}
			}
			return "admin", nil
		},
		Probe: func(context.Context, config.Config) error { return nil }, Control: func(_ context.Context, action string) error { calls = append(calls, action); return nil }, Running: func() bool { return false }}
	if err := config.Save(a.ConfigPath, config.Default()); err != nil {
		t.Fatal(err)
	}
	return a, &calls
}

func readState(t *testing.T, a *App) State {
	t.Helper()
	r := httptest.NewRequest("GET", "http://nas/api.cgi?action=status", nil)
	r.Header.Set("Cookie", "id=fixture")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var state State
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func saveRequest(a *App, state State, c config.Config, csrf, origin string) *httptest.ResponseRecorder {
	data, _ := json.Marshal(map[string]any{"config": c, "revision": state.Revision})
	r := httptest.NewRequest("POST", "http://nas/api.cgi?action=save", bytes.NewReader(data))
	r.Header.Set("Cookie", "id=fixture")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-PLB-CSRF", csrf)
	r.Header.Set("Origin", origin)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func TestManagementRequiresSessionAndCSRF(t *testing.T) {
	a, calls := fixture(t)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "http://nas/api.cgi?action=status", nil))
	if w.Code != 401 {
		t.Fatal("anonymous management access allowed")
	}
	s := readState(t, a)
	for _, test := range []struct{ csrf, origin string }{{"wrong", "http://nas"}, {s.CSRF, "https://other.example"}} {
		if w := saveRequest(a, s, s.Config, test.csrf, test.origin); w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(*calls) != 0 {
		t.Fatal("unauthorized request changed service")
	}
}

func TestSaveValidatedConfigAndRestart(t *testing.T) {
	a, calls := fixture(t)
	s := readState(t, a)
	c := s.Config
	c.MaxConnections = 64
	w := saveRequest(a, s, c, s.CSRF, "http://nas")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	actual, err := config.Load(a.ConfigPath)
	if err != nil || actual != c {
		t.Fatalf("%+v %v", actual, err)
	}
	if !slices.Equal(*calls, []string{"stop", "start"}) {
		t.Fatal(*calls)
	}
	info, _ := os.Stat(a.ConfigPath)
	if info.Mode().Perm() != 0600 {
		t.Fatal("configuration permissions changed")
	}
}

func TestSaveRejectsStaleRevisionAndInvalidUpstream(t *testing.T) {
	a, calls := fixture(t)
	s := readState(t, a)
	c := s.Config
	c.MaxConnections++
	if err := config.Save(a.ConfigPath, c); err != nil {
		t.Fatal(err)
	}
	if w := saveRequest(a, s, s.Config, s.CSRF, "http://nas"); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	s = readState(t, a)
	c = s.Config
	c.UpstreamURL = "http://203.0.113.1:32400"
	if w := saveRequest(a, s, c, s.CSRF, "http://nas"); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatal("invalid configuration restarted service")
	}
}

func TestOccupiedPortDoesNotStopProxy(t *testing.T) {
	a, calls := fixture(t)
	s := readState(t, a)
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c := s.Config
	c.ListenPort = l.Addr().(*net.TCPAddr).Port
	w := saveRequest(a, s, c, s.CSRF, "http://nas")
	if w.Code != 409 || len(*calls) != 0 {
		t.Fatal(w.Code, w.Body.String(), *calls)
	}
}

func TestStartupFailureRestoresPreviousConfigurationAndService(t *testing.T) {
	a, calls := fixture(t)
	a.Running = func() bool { return true }
	starts := 0
	a.Control = func(_ context.Context, action string) error {
		*calls = append(*calls, action)
		if action == "start" {
			starts++
			if starts == 1 {
				return errors.New("bind: address already in use")
			}
		}
		return nil
	}
	s := readState(t, a)
	c := s.Config
	c.MaxConnections++
	w := saveRequest(a, s, c, s.CSRF, "http://nas")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "已恢复原配置") || !strings.Contains(w.Body.String(), `"code":500`) {
		t.Fatal(w.Code, w.Body.String())
	}
	actual, err := config.Load(a.ConfigPath)
	if err != nil || actual != s.Config {
		t.Fatal(actual, err)
	}
	if !slices.Equal(*calls, []string{"stop", "start", "stop", "start"}) {
		t.Fatal(*calls)
	}
}

func TestAdministratorGroupAndSessionBoundToken(t *testing.T) {
	groups := "users:x:100:\nadministrators:x:101:Admin,operator\n"
	if !isAdministrator(groups, "admin") || !isAdministrator(groups, "operator") || isAdministrator(groups, "visitor") {
		t.Fatal("invalid administrator membership")
	}
	key := []byte("test")
	if csrfToken(key, "admin", "id=first") == csrfToken(key, "admin", "id=second") {
		t.Fatal("CSRF token not bound to session")
	}
}
