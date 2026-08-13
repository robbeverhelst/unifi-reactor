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
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testConsoleURL = "https://console.example.test"
	testAPIKey     = "key"
)

func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

// The console URL alone must keep behaving exactly as it did before the fast
// path existed: an install that upgrades without changing anything polls, and
// opens no socket and no console session.
func TestConfigFromEnvLeavesTheFastPathOffByDefault(t *testing.T) {
	cfg, enabled, err := ConfigFromEnv(env(map[string]string{
		envURL:    testConsoleURL,
		envAPIKey: testAPIKey,
	}))
	if err != nil || !enabled {
		t.Fatalf("expected the provider to be enabled, got enabled %v err %v", enabled, err)
	}
	if cfg.Webhook.Enabled || cfg.Webhook.Register {
		t.Errorf("expected the fast path and self-registration off, got %+v", cfg.Webhook)
	}
	if cfg.PollInterval != DefaultPollInterval {
		t.Errorf("expected the default poll interval, got %s", cfg.PollInterval)
	}
	if err := cfg.Webhook.Validate(); err != nil {
		t.Errorf("a default configuration must be valid, got %v", err)
	}
}

func TestConfigFromEnvWithoutAConsoleDisablesTheProvider(t *testing.T) {
	// Even with the webhook switched on: without a console there is nothing to
	// re-observe, so there is nothing to be fast about.
	_, enabled, err := ConfigFromEnv(env(map[string]string{envWebhookEnabled: envTrue}))
	if err != nil || enabled {
		t.Errorf("expected the provider disabled and no error, got enabled %v err %v", enabled, err)
	}
}

func TestConfigFromEnvRequiresAnAPIKeyAlongsideAURL(t *testing.T) {
	if _, _, err := ConfigFromEnv(env(map[string]string{envURL: testConsoleURL})); err == nil {
		t.Error("expected a URL without an API key to be an error")
	}
}

// The key file wins over the environment, and is re-read on every use — that
// is what makes rotating a mounted Secret take effect without a restart.
func TestConfigFromEnvPrefersTheKeyFileAndRereadsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "UNIFI_API_KEY")
	if err := os.WriteFile(path, []byte("first-key\n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}

	cfg, enabled, err := ConfigFromEnv(env(map[string]string{
		envURL:        testConsoleURL,
		envAPIKeyFile: path,
		envAPIKey:     "the-environment-key",
	}))
	if err != nil || !enabled {
		t.Fatalf("expected the provider enabled, got enabled %v err %v", enabled, err)
	}

	got, err := cfg.APIKey()
	if err != nil || got != "first-key" {
		t.Fatalf("expected the file to win over the environment, got %q (%v)", got, err)
	}

	// Rotation: the kubelet replaces the file in place and the next use picks
	// it up, with no restart and no re-reading of the configuration.
	if err := os.WriteFile(path, []byte("rotated-key\n"), 0o600); err != nil {
		t.Fatalf("rotating the key file: %v", err)
	}
	if got, err = cfg.APIKey(); err != nil || got != "rotated-key" {
		t.Errorf("expected the rotated key to be picked up, got %q (%v)", got, err)
	}
}

// An unreadable or empty key file is a deployment mistake, and it must be
// fatal at startup rather than surfacing as a failed poll later.
func TestConfigFromEnvRejectsAnUnusableKeyFile(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "UNIFI_API_KEY")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("writing the key file: %v", err)
	}

	for name, path := range map[string]string{
		"missing":    filepath.Join(t.TempDir(), "does-not-exist"),
		"empty file": empty,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := ConfigFromEnv(env(map[string]string{envURL: testConsoleURL, envAPIKeyFile: path}))
			if err == nil {
				t.Error("expected an unusable key file to be reported at startup")
			}
		})
	}
}

// Without a file, the environment key is used and is fixed for the process.
func TestConfigFromEnvFallsBackToTheEnvironmentKey(t *testing.T) {
	cfg, _, err := ConfigFromEnv(env(map[string]string{envURL: testConsoleURL, envAPIKey: testAPIKey}))
	if err != nil {
		t.Fatalf("reading the configuration: %v", err)
	}
	if got, err := cfg.APIKey(); err != nil || got != testAPIKey {
		t.Errorf("expected the environment key, got %q (%v)", got, err)
	}
}

func TestConfigFromEnvReadsTheFastPathSettings(t *testing.T) {
	cfg, enabled, err := ConfigFromEnv(env(map[string]string{
		envURL:                testConsoleURL,
		envAPIKey:             testAPIKey,
		envPollInterval:       "15s",
		envWebhookEnabled:     envTrue,
		envWebhookToken:       testToken,
		envWebhookBindAddress: ":9999",
		envWebhookPath:        testCustomPath,
		envWebhookMinInterval: "2s",
		envWebhookRegister:    envTrue,
		envWebhookPublicURL:   "http://reactor.example.test:9999/hooks/reactor",
		envWebhookRuleTitle:   "reactor-fast-path",
		envUsername:           testConsoleUser,
		envPassword:           testConsolePassword,
	}))
	if err != nil || !enabled {
		t.Fatalf("expected the provider enabled, got enabled %v err %v", enabled, err)
	}
	if cfg.PollInterval != 15*time.Second || cfg.Webhook.MinObserveInterval != 2*time.Second {
		t.Errorf("expected the configured intervals, got %s and %s", cfg.PollInterval, cfg.Webhook.MinObserveInterval)
	}
	if cfg.Webhook.BindAddress != ":9999" || cfg.Webhook.Path != testCustomPath {
		t.Errorf("expected the configured endpoint, got %s%s", cfg.Webhook.BindAddress, cfg.Webhook.Path)
	}
	if cfg.Webhook.RuleTitle != "reactor-fast-path" {
		t.Errorf("expected the configured rule title, got %q", cfg.Webhook.RuleTitle)
	}
	if err := cfg.Webhook.Validate(); err != nil {
		t.Errorf("expected a valid configuration, got %v", err)
	}
}

// Flags that default off must need the exact word, so a plausible-looking
// value cannot switch on a listening socket or a write to the console.
func TestConfigFromEnvOnlyTreatsTrueAsTrue(t *testing.T) {
	for _, value := range []string{"1", "yes", "TRUE", "True", "on", " true"} {
		cfg, _, err := ConfigFromEnv(env(map[string]string{
			envURL:            testConsoleURL,
			envAPIKey:         "key",
			envWebhookEnabled: value,
		}))
		if err != nil {
			t.Fatalf("reading the configuration: %v", err)
		}
		if cfg.Webhook.Enabled {
			t.Errorf("%q must not enable the fast path", value)
		}
	}
}

func TestConfigFromEnvRejectsUnparseableValues(t *testing.T) {
	for name, override := range map[string]map[string]string{
		"poll interval":       {envPollInterval: "half a minute"},
		"min interval":        {envWebhookMinInterval: "soon"},
		"low battery":         {envLowBattery: "thirty"},
		"critical battery":    {envCriticalBattery: "10%"},
		"poll interval units": {envPollInterval: "30"},
	} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{envURL: testConsoleURL, envAPIKey: testAPIKey}
			maps.Copy(values, override)
			if _, _, err := ConfigFromEnv(env(values)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestWebhookConfigValidate(t *testing.T) {
	valid := WebhookConfig{
		Enabled:     true,
		Path:        DefaultWebhookPath,
		Token:       testToken,
		Register:    true,
		Username:    testConsoleUser,
		Password:    testConsolePassword,
		PublicURL:   "http://reactor.example.test:9090/webhooks/unifi",
		BindAddress: DefaultWebhookBindAddress,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected a valid configuration, got %v", err)
	}

	for name, mutate := range map[string]func(*WebhookConfig){
		"no token":            func(w *WebhookConfig) { w.Token = "" },
		"relative path":       func(w *WebhookConfig) { w.Path = "webhooks/unifi" },
		"negative interval":   func(w *WebhookConfig) { w.MinObserveInterval = -time.Second },
		"no console user":     func(w *WebhookConfig) { w.Username = "" },
		"no console password": func(w *WebhookConfig) { w.Password = "" },
		"no public url":       func(w *WebhookConfig) { w.PublicURL = "" },
		"loopback public url": func(w *WebhookConfig) { w.PublicURL = "http://127.0.0.1:9090/webhooks/unifi" },
		"registration alone":  func(w *WebhookConfig) { w.Enabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			broken := valid
			mutate(&broken)
			if err := broken.Validate(); err == nil {
				t.Error("expected an error")
			}
		})
	}

	// Console credentials and a public URL only matter to self-registration.
	receiverOnly := WebhookConfig{Enabled: true, Path: DefaultWebhookPath, Token: testToken}
	if err := receiverOnly.Validate(); err != nil {
		t.Errorf("a receiver without self-registration must be valid, got %v", err)
	}
}
