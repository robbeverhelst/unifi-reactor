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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// capturedGateway serves the real captured stat/device response from testdata.
func capturedGateway(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "unifi", "api", "stat-device-gateway.json"))
	if err != nil {
		t.Fatalf("reading captured payload: %v", err)
	}
	return b
}

func TestObserveWANStateAgainstCapturedPayload(t *testing.T) {
	payload := capturedGateway(t)
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-KEY")
		gotPath = r.URL.Path
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", "", false)
	state, err := c.ObserveWANState(context.Background())
	if err != nil {
		t.Fatalf("ObserveWANState: %v", err)
	}
	if state["wan"] != "primary" {
		t.Fatalf("expected wan=primary from captured payload (WAN1 active), got %q", state["wan"])
	}
	if gotKey != "test-key" {
		t.Fatalf("expected X-API-KEY header, got %q", gotKey)
	}
	if gotPath != "/proxy/network/api/s/default/stat/device" {
		t.Fatalf("unexpected path %q", gotPath)
	}
}

func TestWANStateBackupWhenWAN2IsUplink(t *testing.T) {
	parsed := deviceStatResponse{}
	parsed.Data = []struct {
		Model string   `json:"model"`
		Type  string   `json:"type"`
		WAN1  *wanPort `json:"wan1"`
		WAN2  *wanPort `json:"wan2"`
	}{
		{Model: "UDMPRO", WAN1: &wanPort{IsUplink: false, Up: false}, WAN2: &wanPort{IsUplink: true, Up: true}},
	}
	state, err := wanStateFromDevices(parsed)
	if err != nil {
		t.Fatalf("wanStateFromDevices: %v", err)
	}
	if state["wan"] != "backup" {
		t.Fatalf("expected wan=backup, got %q", state["wan"])
	}
}

func TestWANStateErrorsWithoutGateway(t *testing.T) {
	if _, err := wanStateFromDevices(deviceStatResponse{}); err == nil {
		t.Fatal("expected error for empty device list")
	}
}
