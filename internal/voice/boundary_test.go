package voice

import (
	"os/exec"
	"strings"
	"testing"
)

func TestEngineHasNoTelephonyOrProviderImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	forbidden := []string{
		"/internal/telephony",
		"/internal/realtime/realtimews",
		"/internal/realtime/livews",
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range forbidden {
			if strings.Contains(dep, bad) {
				t.Fatalf("voice engine depends on %s; it must stay protocol- and transport-neutral", dep)
			}
		}
	}
}
