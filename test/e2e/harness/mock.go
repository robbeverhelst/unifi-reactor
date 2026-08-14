//go:build e2e

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

package harness

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// MockImage is the tag the UniFi mock is built and loaded under.
	MockImage = "example.com/mock-unifi:e2e"
	// MockPort is the port the mock serves on, in the pod and in the Service.
	MockPort = 9443
	// mockNodePort is fixed so the suite can reach the mock from outside the
	// cluster over the Kind port mapping, without a port-forward to keep alive.
	mockNodePort = 30943
	// MockName is the name of the mock's Deployment and Service.
	MockName = "mock-unifi"
)

// mockDockerfile packages a statically linked mock next to the captured
// payloads it serves. It is written into a build context assembled at test
// time rather than living in the repository, because the repository's
// .dockerignore keeps testdata out of every build — deliberately, so that
// captured payloads cannot end up in a shipped image.
const mockDockerfile = `FROM scratch
WORKDIR /
COPY mock-unifi /mock-unifi
COPY api /testdata/unifi/api
ENTRYPOINT ["/mock-unifi"]
`

// BuildMockImage compiles hack/mock-unifi for the cluster's architecture and
// packages it with the captured payloads from testdata/.
func BuildMockImage(log io.Writer, dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "api"), 0o755); err != nil {
		return err
	}
	// Built for the node's architecture, which is the host's: Kind nodes run
	// the same platform as the machine hosting them.
	env := []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + runtime.GOARCH}
	binary := filepath.Join(dir, "mock-unifi")
	if _, err := RunEnv(log, ProjectDir(), env, "go", "build", "-o", binary, "./hack/mock-unifi"); err != nil {
		return err
	}

	source := filepath.Join(ProjectDir(), "testdata", "unifi", "api")
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		payload, err := os.ReadFile(filepath.Join(source, entry.Name())) // #nosec G304 -- committed fixtures
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "api", entry.Name()), payload, 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(mockDockerfile), 0o600); err != nil {
		return err
	}
	_, err = Run(log, dir, "docker", "build", "-t", MockImage, ".")
	return err
}

// MockManifest is the mock's Deployment and Service in one namespace. The
// Service is a NodePort so the suite can drive rehearsals over Kind's port
// mapping while the operator reaches the same mock by cluster DNS.
func MockManifest(namespace string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      containers:
        - name: mock
          image: %[3]s
          imagePullPolicy: Never
          ports: [{containerPort: %[4]d}]
          readinessProbe:
            httpGet: {path: /proxy/network/api/s/default/stat/device, port: %[4]d}
            periodSeconds: 1
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  type: NodePort
  selector: {app: %[1]s}
  ports:
    - port: %[4]d
      targetPort: %[4]d
      nodePort: %[5]d
`, MockName, namespace, MockImage, MockPort, mockNodePort)
}

// MockURL is how the operator reaches the mock from inside the cluster.
func MockURL(namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", MockName, namespace, MockPort)
}

// Mock rehearses console state from outside the cluster.
type Mock struct {
	// BaseURL reaches the mock's NodePort through Kind's port mapping.
	BaseURL string
	Log     io.Writer
}

// NewMock addresses the mock over the Kind node's mapped port.
func NewMock(log io.Writer) *Mock {
	return &Mock{BaseURL: fmt.Sprintf("http://127.0.0.1:%d", mockNodePort), Log: log}
}

// WAN puts the gateway on "primary" or "backup", whichever it was on before.
// The absolute form matters here rather than /flip's toggle: a spec that says
// "make sure it is on backup" must not depend on where the one before it left
// the console.
//
// The variant is left at the mock's default, which moves every WAN signal at
// once. Which shape a real failover takes is still unobserved (#34), and these
// suites are about what Reactor does with a reported state rather than about
// how the state is derived — the variants are covered where the derivation is,
// in the provider's own tests.
func (m *Mock) WAN(uplink string) error { return m.post("/wan?link=" + uplink) }

// UPS drives the power rehearsal: mode=battery|mains, level=0-100,
// present=false to make the UPS drop off the console entirely.
func (m *Mock) UPS(query string) error { return m.post("/ups?" + query) }

// Internet drives the console's www subsystem: status=ok|warning|error, or
// present=false to remove the subsystem so the internet key vanishes rather
// than reporting a value.
//
// It is the one rehearsal that cannot be reached through the WAN controls at
// all, which is the point of the key: the link stays up, the uplink is
// unchanged, and there is no internet.
func (m *Mock) Internet(query string) error { return m.post("/internet?" + query) }

// Quality drives the live uplink's uptime stats: availability=<percent>,
// latency=<ms>, present=false for an uplink reporting no numbers, reset=true
// to go back to the capture.
func (m *Mock) Quality(query string) error { return m.post("/quality?" + query) }

// Device drives one adopted device's fleet fields, which the devices and
// device.<name> keys are derived from: name=<slug> plus state=online|offline,
// adopted=<bool>, rename=<new name>, or present=false to take it off the
// console entirely. reset=true puts every device back to the capture.
//
// A device is addressed by the slug of the name it was CAPTURED under, even
// after a rename, which is what makes the rename rehearsal reversible.
func (m *Mock) Device(query string) error { return m.post("/device?" + query) }

// Firmware drives the upgrade fields: upgradable=<bool>, eol=<bool>,
// name=<slug> to move one device rather than all of them, present=false to stop
// reporting the field at all — which is the state every committed capture is
// in, and therefore the mock's own default.
func (m *Mock) Firmware(query string) error { return m.post("/firmware?" + query) }

// Temperature drives the thermal fields: celsius=<reading>, overheating=<bool>,
// general=true for the single-value form, present=false for a device reporting
// no thermals. Nothing is served until this is called, because no capture
// carries a thermal field.
func (m *Mock) Temperature(query string) error { return m.post("/temperature?" + query) }

// WiFi drives the wlan subsystem's AP counts, which the wifi key is derived
// from: adopted=<n>, disconnected=<n>, present=false to remove the subsystem.
// status=<word> moves the console's own wording without moving the counts,
// which is how the disagreement path is rehearsed.
func (m *Mock) WiFi(query string) error { return m.post("/wifi?" + query) }

// PoE drives the PoE budget and draw: watts=<n>, budget=<n>, silent=true for a
// powered port that reports no wattage, present=false for no PoE fields at all.
func (m *Mock) PoE(query string) error { return m.post("/poe?" + query) }

// Outlets drives the UPS outlet table: outlet=<n>&state=on|off for one outlet,
// group=<n>&state=... for a whole relay group, outlet=<n>&label=<name> to name
// one, present=false for a UPS reporting no outlets.
//
// switching=individual|group is the one that matters: it decides whether asking
// for one outlet moves that outlet or its entire relay group, which is the
// question #23 is deferred on and the two readings hypothesis H1 on #60 tells
// apart. Reactor only ever reads these.
func (m *Mock) Outlets(query string) error { return m.post("/outlets?" + query) }

// Reachable reports whether the mock is answering yet.
func (m *Mock) Reachable() error {
	return m.request(http.MethodGet, "/proxy/network/api/s/default/stat/device")
}

func (m *Mock) post(path string) error { return m.request(http.MethodPost, path) }

func (m *Mock) request(method, path string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(method, m.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, body)
	}
	_, _ = fmt.Fprintf(m.Log, "mock %s %s -> %s", method, path, body)
	return nil
}
