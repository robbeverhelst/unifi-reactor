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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultPollInterval is how often state is observed when nothing says
// otherwise. Polling is the mechanism of record, so this interval — not the
// webhook fast path — is what bounds correctness.
const DefaultPollInterval = 30 * time.Second

// envTrue is the only value that turns a boolean environment flag on. Anything
// else, including "1" and "yes", leaves the flag off: for settings that default
// off on purpose, a typo must not enable them.
const envTrue = "true"

// The environment variables the provider reads, named in one place so the
// contract with the Helm chart is stated once.
const (
	envURL                = "UNIFI_URL"
	envAPIKey             = "UNIFI_API_KEY"
	envAPIKeyFile         = "UNIFI_API_KEY_FILE" // #nosec G101 -- variable name, not a credential
	envSite               = "UNIFI_SITE"
	envInsecureSkipVerify = "UNIFI_INSECURE_SKIP_VERIFY"
	envPollInterval       = "UNIFI_POLL_INTERVAL"
	envLowBattery         = "UNIFI_UPS_LOW_BATTERY_PERCENT"
	envCriticalBattery    = "UNIFI_UPS_CRITICAL_BATTERY_PERCENT"
	envShortRuntime       = "UNIFI_UPS_SHORT_RUNTIME_SECONDS"
	envCriticalRuntime    = "UNIFI_UPS_CRITICAL_RUNTIME_SECONDS"
	envHighLoad           = "UNIFI_UPS_HIGH_LOAD_PERCENT"
	envMinAvailability    = "UNIFI_WAN_QUALITY_MIN_AVAILABILITY_PERCENT"
	envMaxLatency         = "UNIFI_WAN_QUALITY_MAX_LATENCY_MS"
	envPerDeviceKeys      = "UNIFI_PER_DEVICE_KEYS"
	envHighTemperature    = "UNIFI_TEMPERATURE_HIGH_CELSIUS"

	envWebhookEnabled     = "UNIFI_WEBHOOK_ENABLED"
	envWebhookBindAddress = "UNIFI_WEBHOOK_BIND_ADDRESS"
	envWebhookPath        = "UNIFI_WEBHOOK_PATH"
	envWebhookToken       = "UNIFI_WEBHOOK_TOKEN" // #nosec G101 -- variable name, not a credential
	envWebhookMinInterval = "UNIFI_WEBHOOK_MIN_INTERVAL"
	envWebhookRegister    = "UNIFI_WEBHOOK_REGISTER"
	envWebhookPublicURL   = "UNIFI_WEBHOOK_PUBLIC_URL"
	envWebhookRuleTitle   = "UNIFI_WEBHOOK_RULE_TITLE"
	envUsername           = "UNIFI_USERNAME"
	envPassword           = "UNIFI_PASSWORD"

	envActionsAllowedWLANs    = "UNIFI_ACTIONS_ALLOWED_WLANS"
	envActionsAllowedPoEPorts = "UNIFI_ACTIONS_ALLOWED_POE_PORTS"
)

// Config is the UniFi provider's install-level configuration. There is one
// UniFi console per Reactor install, so this comes from the environment (Helm
// values) rather than from an Automation.
type Config struct {
	URL string
	// APIKey is resolved per request, not held from startup, so rotating a
	// mounted credential takes effect on the next poll.
	APIKey                 APIKey
	Site                   string
	InsecureSkipVerify     bool
	PollInterval           time.Duration
	LowBatteryPercent      int
	CriticalBatteryPercent int
	ShortRuntimeSeconds    int
	CriticalRuntimeSeconds int
	HighLoadPercent        float64
	MinAvailabilityPercent float64
	MaxLatencyMs           float64
	HighTemperatureCelsius float64
	// PerDeviceKeys opts into a device.<name> key per adopted device. Off by
	// default: it is the only setting that changes how many series an install
	// publishes rather than what any of them mean.
	PerDeviceKeys bool
	Webhook       WebhookConfig
	// Actions is what this install allows Reactor to write to the console.
	// Empty by default, which refuses every console write action. See
	// write.go: the console is the one thing Reactor touches that is not a
	// Kubernetes object, so the decision about what may be changed on it is the
	// operator's and is taken at install time.
	Actions ActionsConfig
}

// ConsoleCredentials returns the UniFi OS local-account username and password.
//
// They live on WebhookConfig because Alarm Manager self-registration needed
// them first, and they are named once here rather than read twice because the
// write actions authenticate exactly the same way: same UniFi OS layer, same
// cookie session, same csrfToken claim, same pair. The API key the poller reads
// state with does not work for either.
func (c Config) ConsoleCredentials() (username, password string) {
	return c.Webhook.Username, c.Webhook.Password
}

// WebhookConfig configures the fast path. Every field here defaults off or to
// something inert: an existing install that upgrades keeps polling and gains
// no new listening socket, no new credentials, and no writes to the console.
type WebhookConfig struct {
	// Enabled starts the receiver. A delivery to it only ever triggers a poll.
	Enabled bool
	// BindAddress is where the receiver listens inside the pod.
	BindAddress string
	// Path is the URL path deliveries are accepted on.
	Path string
	// Token is the shared secret every delivery must present. Without it the
	// receiver accepts nothing, so it is not optional.
	Token string
	// MinObserveInterval floors the time between two observations, bounding how
	// much console traffic a burst of deliveries can cause.
	MinObserveInterval time.Duration

	// Register lets Reactor create its own Alarm Manager rule on the console.
	Register bool
	// PublicURL is the address the console should post to. Reactor cannot
	// derive it: only the operator knows how the pod is exposed.
	PublicURL string
	// RuleTitle is the title Reactor gives the rule it creates, and the title
	// it looks for to decide the rule already exists.
	RuleTitle string
	// Username and Password are UniFi OS console credentials. The Alarm Manager
	// API sits at the UniFi OS layer and rejects the API key the poller uses.
	Username string
	Password string
}

// ConfigFromEnv reads the provider configuration. The second return value
// reports whether the provider is configured at all; lookup is injected so the
// mapping can be tested without mutating process state.
func ConfigFromEnv(lookup func(string) string) (Config, bool, error) {
	cfg := Config{
		URL:                    lookup(envURL),
		Site:                   lookup(envSite),
		InsecureSkipVerify:     lookup(envInsecureSkipVerify) == envTrue,
		PollInterval:           DefaultPollInterval,
		LowBatteryPercent:      DefaultLowBatteryPercent,
		CriticalBatteryPercent: DefaultCriticalBatteryPercent,
		ShortRuntimeSeconds:    DefaultShortRuntimeSeconds,
		CriticalRuntimeSeconds: DefaultCriticalRuntimeSeconds,
		HighLoadPercent:        DefaultHighLoadPercent,
		MinAvailabilityPercent: DefaultMinAvailabilityPercent,
		MaxLatencyMs:           DefaultMaxLatencyMs,
		HighTemperatureCelsius: DefaultHighTemperatureCelsius,
		PerDeviceKeys:          lookup(envPerDeviceKeys) == envTrue,
		Webhook: WebhookConfig{
			Enabled:            lookup(envWebhookEnabled) == envTrue,
			BindAddress:        DefaultWebhookBindAddress,
			Path:               DefaultWebhookPath,
			Token:              lookup(envWebhookToken),
			MinObserveInterval: DefaultMinObserveInterval,
			Register:           lookup(envWebhookRegister) == envTrue,
			PublicURL:          lookup(envWebhookPublicURL),
			RuleTitle:          DefaultAlarmRuleTitle,
			Username:           lookup(envUsername),
			Password:           lookup(envPassword),
		},
		Actions: ActionsConfig{
			AllowedWLANs:    splitList(lookup(envActionsAllowedWLANs)),
			AllowedPoEPorts: splitList(lookup(envActionsAllowedPoEPorts)),
		},
	}

	// No console configured is not an error; it is how Reactor runs with the
	// UniFi provider switched off.
	if cfg.URL == "" {
		return Config{}, false, nil
	}

	apiKey, err := apiKeyFromEnv(lookup)
	if err != nil {
		return Config{}, false, err
	}
	cfg.APIKey = apiKey

	for _, field := range []struct {
		env    string
		target *time.Duration
	}{
		{envPollInterval, &cfg.PollInterval},
		{envWebhookMinInterval, &cfg.Webhook.MinObserveInterval},
	} {
		raw := lookup(field.env)
		if raw == "" {
			continue
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, false, fmt.Errorf("invalid %s %q: %w", field.env, raw, err)
		}
		*field.target = parsed
	}

	for _, field := range []struct {
		env    string
		target *int
	}{
		{envLowBattery, &cfg.LowBatteryPercent},
		{envCriticalBattery, &cfg.CriticalBatteryPercent},
		{envShortRuntime, &cfg.ShortRuntimeSeconds},
		{envCriticalRuntime, &cfg.CriticalRuntimeSeconds},
	} {
		raw := lookup(field.env)
		if raw == "" {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, false, fmt.Errorf("invalid %s %q: %w", field.env, raw, err)
		}
		*field.target = parsed
	}

	// Thresholds on a measured quantity rather than a percentage of charge, so
	// they are floats: an operator on a link that sits at 99.5% needs to be
	// able to say so.
	for _, field := range []struct {
		env    string
		target *float64
	}{
		{envMinAvailability, &cfg.MinAvailabilityPercent},
		{envMaxLatency, &cfg.MaxLatencyMs},
		{envHighLoad, &cfg.HighLoadPercent},
		{envHighTemperature, &cfg.HighTemperatureCelsius},
	} {
		raw := lookup(field.env)
		if raw == "" {
			continue
		}
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, false, fmt.Errorf("invalid %s %q: %w", field.env, raw, err)
		}
		*field.target = parsed
	}

	for _, field := range []struct {
		env    string
		target *string
	}{
		{envWebhookBindAddress, &cfg.Webhook.BindAddress},
		{envWebhookPath, &cfg.Webhook.Path},
		{envWebhookRuleTitle, &cfg.Webhook.RuleTitle},
	} {
		if raw := lookup(field.env); raw != "" {
			*field.target = raw
		}
	}

	return cfg, true, nil
}

// apiKeyFromEnv resolves where the API key comes from, and fails fast if it
// cannot be read at all. UNIFI_API_KEY_FILE points at a mounted Secret and is
// re-read on every poll, so rotating the key takes effect without a restart;
// UNIFI_API_KEY holds the key in the environment, where it is fixed for the
// life of the process and rotation means restarting the pod.
//
// The file is read once here purely to fail at startup rather than at the
// first poll: a path that cannot be read is a deployment mistake, and finding
// out immediately is worth the one read.
func apiKeyFromEnv(lookup func(string) string) (APIKey, error) {
	if path := lookup(envAPIKeyFile); path != "" {
		key := FileAPIKey(path)
		if _, err := key(); err != nil {
			return nil, err
		}
		return key, nil
	}
	if key := lookup(envAPIKey); key != "" {
		return StaticAPIKey(key), nil
	}
	return nil, errors.New("UNIFI_URL is set but neither UNIFI_API_KEY_FILE nor UNIFI_API_KEY is")
}

// Validate reports why the fast path cannot start. Callers are expected to log
// the reason and carry on polling: a misconfigured optimization must never take
// the mechanism of record down with it.
func (w WebhookConfig) Validate() error {
	if !w.Enabled {
		if w.Register {
			return errors.New("webhook self-registration needs the receiver enabled too")
		}
		return nil
	}
	if w.Token == "" {
		return errors.New("the webhook fast path needs a shared secret; set UNIFI_WEBHOOK_TOKEN")
	}
	if !strings.HasPrefix(w.Path, "/") {
		return fmt.Errorf("the webhook path must start with a slash, got %q", w.Path)
	}
	if w.MinObserveInterval < 0 {
		return fmt.Errorf("the minimum observe interval cannot be negative, got %s", w.MinObserveInterval)
	}
	if !w.Register {
		return nil
	}
	if w.Username == "" || w.Password == "" {
		return errors.New("webhook self-registration needs UniFi OS console credentials " +
			"(UNIFI_USERNAME and UNIFI_PASSWORD); the API key the poller uses does not work on the Alarm Manager API")
	}
	return validateCallbackURL(w.PublicURL)
}
