package webui

import (
	"errors"
	"os/exec"
	"testing"
)

func TestDSMIdentityUsesDocumentedStdoutEvenWithNonzeroExit(t *testing.T) {
	output, err := exec.Command("sh", "-c", "printf admin; exit 1").Output()
	if err == nil {
		t.Fatal("fixture must return a nonzero exit status")
	}
	name, err := dsmIdentity(output, err)
	if err != nil || name != "admin" {
		t.Fatalf("name=%q error=%v", name, err)
	}
}

func TestDSMIdentityRejectsEmptyOutputAndLaunchErrors(t *testing.T) {
	for _, test := range []struct {
		name, script string
		status       int
	}{
		{"missing session", "exit 1", 401},
		{"unexpected output", "printf 'admin\\nother'; exit 0", 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, commandErr := exec.Command("sh", "-c", test.script).Output()
			_, err := dsmIdentity(output, commandErr)
			var api apiError
			if !errors.As(err, &api) || api.status != test.status {
				t.Fatal(err)
			}
		})
	}
	if _, err := dsmIdentity([]byte("admin"), exec.ErrNotFound); err == nil {
		t.Fatal("execution failure must not authenticate a user")
	}
}
