/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package unifi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hack/verify-testdata.sh requires the carrier fields in a capture to equal the
// placeholder the capture script writes, rather than forbidding the carriers it
// has seen. Required-to-equal is what makes the guard worth having: it catches
// every carrier from every future capture, and it never has to name one, where
// a list of forbidden names would have to carry the real value in a public file
// to protect it (#94).
//
// A guard nothing exercises is a guard that can quietly stop matching — move
// the check out of the loop, rename the variable it reads, and every capture
// still passes. So these tests drive the script itself, in all three
// directions: a real-looking carrier is rejected, the placeholder is accepted,
// and the committed captures pass every guard in the file.
//
// The placeholders are read out of hack/capture-unifi.sh, which is where the
// script under test reads them from too, so no test here has to repeat a value
// that would then drift.

const (
	verifyTestdataScript = "verify-testdata.sh"
	captureScript        = "capture-unifi.sh"
)

func hackScript(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "hack", name)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("hack/%s is not reachable from here: %v", name, err)
	}
	return path
}

// runVerifyTestdata runs the script against root, or against the repository
// when root is empty, and returns its combined output.
func runVerifyTestdata(t *testing.T, root string) (string, error) {
	t.Helper()
	args := []string{hackScript(t, verifyTestdataScript)}
	if root != "" {
		args = append(args, root)
	}
	out, err := exec.Command("bash", args...).CombinedOutput()
	return string(out), err
}

// placeholderCarrier reads one of the ISP placeholders out of the capture
// script, the same way the guard under test does.
func placeholderCarrier(t *testing.T, name string) string {
	t.Helper()
	script, err := os.ReadFile(hackScript(t, captureScript))
	if err != nil {
		t.Fatalf("reading hack/%s: %v", captureScript, err)
	}
	match := regexp.MustCompile(`(?m)^` + name + `='([^']*)'`).FindSubmatch(script)
	if match == nil || len(match[1]) == 0 {
		t.Fatalf("hack/%s no longer declares %s; the carrier guard has nothing to require", captureScript, name)
	}
	return string(match[1])
}

// captureWithCarrier writes a throwaway tree holding one capture whose only
// interesting content is its carrier fields. Everything else about it passes
// every other guard in the script: no secret fields, no routable addresses, no
// real MACs.
func captureWithCarrier(t *testing.T, name, organization string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "testdata", "unifi", "api")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("building a throwaway capture tree: %v", err)
	}
	fixture := fmt.Sprintf(
		`{"meta":{"rc":"ok"},"data":[{"subsystem":"wan","status":"ok",`+
			`"wan_ip":"203.0.113.10","isp_name":%q,"isp_organization":%q}]}`, name, organization)
	if err := os.WriteFile(filepath.Join(dir, "stat-health.json"), []byte(fixture), 0o644); err != nil {
		t.Fatalf("writing the throwaway capture: %v", err)
	}
	return root
}

// The carrier here is invented, and that is the point: the guard is not
// checking against a list of known carriers, so any name that is not the
// placeholder has to trip it.
func TestACarrierThatIsNotThePlaceholderIsRejected(t *testing.T) {
	root := captureWithCarrier(t, "Northwind Broadband", "Northwind Broadband PLC")

	out, err := runVerifyTestdata(t, root)
	if err == nil {
		t.Fatalf("verify-testdata.sh accepted a capture carrying a real-looking carrier:\n%s", out)
	}
	for _, want := range []string{"isp_name", "isp_organization", "stat-health.json", "capture-unifi.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure should mention %q, so it says what is wrong and what to do; got:\n%s", want, out)
		}
	}
	// Naming the offending value would put a real carrier in the CI log of a
	// public repository, which is the thing this guard exists to prevent.
	if strings.Contains(out, "Northwind") {
		t.Errorf("the failure echoed the carrier it caught, got:\n%s", out)
	}
}

func TestThePlaceholderCarrierIsAccepted(t *testing.T) {
	root := captureWithCarrier(t,
		placeholderCarrier(t, "ISP"), placeholderCarrier(t, "ISP_ORG"))

	if out, err := runVerifyTestdata(t, root); err != nil {
		t.Fatalf("verify-testdata.sh rejected the placeholder its own capture script writes:\n%s", out)
	}
}

func TestTheCommittedCapturesPassEveryGuard(t *testing.T) {
	if out, err := runVerifyTestdata(t, ""); err != nil {
		t.Fatalf("the committed captures do not pass hack/verify-testdata.sh:\n%s", out)
	}
}
