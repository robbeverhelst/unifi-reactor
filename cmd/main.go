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

package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	reactorv1alpha1 "github.com/robbeverhelst/unifi-reactor/api/v1alpha1"
	"github.com/robbeverhelst/unifi-reactor/internal/controller"
	"github.com/robbeverhelst/unifi-reactor/internal/engine"
	"github.com/robbeverhelst/unifi-reactor/internal/providers/unifi"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// targetAdmissionWarnings logs API-server warnings under their own name and at
// debug level, making clear they describe the target resource rather than the
// operator, and keeping them out of the INFO stream that reports action
// results.
type targetAdmissionWarnings struct {
	log logr.Logger
}

func (w targetAdmissionWarnings) HandleWarningHeader(code int, _ string, text string) {
	if code != 299 || text == "" {
		return
	}
	w.log.V(1).Info("API server warning about a targeted resource; the request itself succeeded", "warning", text)
}

// unifiAPIKey resolves where the API key comes from, and fails fast if it
// cannot be read at all. UNIFI_API_KEY_FILE points at a mounted Secret and is
// re-read on every poll, so rotating the key takes effect without a restart;
// UNIFI_API_KEY holds the key in the environment, where it is fixed for the
// life of the process and rotation means restarting the pod.
func unifiAPIKey() (unifi.APIKey, error) {
	if path := os.Getenv("UNIFI_API_KEY_FILE"); path != "" {
		key := unifi.FileAPIKey(path)
		if _, err := key(); err != nil {
			return nil, err
		}
		return key, nil
	}
	if key := os.Getenv("UNIFI_API_KEY"); key != "" {
		return unifi.StaticAPIKey(key), nil
	}
	return nil, errors.New("UNIFI_URL is set but neither UNIFI_API_KEY_FILE nor UNIFI_API_KEY is")
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(reactorv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// unifiDebounce reads how long a changed UniFi value must hold before Reactor
// acts on it. The keys arrive as opaque data so the engine stays free of any
// knowledge of what they mean; deciding that a battery threshold should settle
// but a WAN failover should not is the provider's call, not the core's.
func unifiDebounce() (engine.DebounceConfig, error) {
	config := engine.DebounceConfig{Default: 1, PerKey: map[string]int{}}

	if raw := os.Getenv("UNIFI_DEBOUNCE_DEFAULT"); raw != "" {
		samples, err := strconv.Atoi(raw)
		if err != nil {
			return config, fmt.Errorf("UNIFI_DEBOUNCE_DEFAULT %q: %w", raw, err)
		}
		config.Default = samples
	}

	for pair := range strings.SplitSeq(os.Getenv("UNIFI_DEBOUNCE_KEYS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, raw, found := strings.Cut(pair, "=")
		if !found {
			return config, fmt.Errorf("UNIFI_DEBOUNCE_KEYS %q is not key=samples", pair)
		}
		samples, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return config, fmt.Errorf("UNIFI_DEBOUNCE_KEYS %q: %w", pair, err)
		}
		config.PerKey[strings.TrimSpace(key)] = samples
	}
	return config, nil
}

// runReleaseClaims hands every claimed target back and exits. It talks to the
// API server directly rather than through a manager's cache, because it has to
// finish inside an uninstall rather than run for as long as the process lives.
func runReleaseClaims() error {
	ctx := ctrl.SetupSignalHandler()
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	return controller.ReleaseAllClaims(logf.IntoContext(ctx, ctrl.Log.WithName("release-claims")), c)
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var releaseClaims bool
	var tlsOpts []func(*tls.Config)
	flag.BoolVar(&releaseClaims, "release-claims", false,
		"Hand every target Reactor holds back to what the Automations referencing it want, drop the "+
			"finalizers, and exit. Run this before removing the operator; the chart's pre-delete hook does it for you.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	if releaseClaims {
		// A one-shot mode, deliberately without a manager: it runs from a
		// pre-delete hook, when the operator's Deployment is about to go away
		// and leader election would have nothing to elect.
		if err := runReleaseClaims(); err != nil {
			setupLog.Error(err, "Failed to release claims")
			os.Exit(1)
		}
		return
	}

	restConfig := ctrl.GetConfigOrDie()
	// Admission warnings describe the resource being acted on — typically a
	// target Deployment's pod spec against its namespace's PodSecurity level —
	// and arrive on requests that succeeded. Left on the default handler they
	// print unstructured at INFO next to action results, where they read like
	// the action failed.
	restConfig.WarningHandler = targetAdmissionWarnings{log: ctrl.Log.WithName("target-warning")}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "b1ad209a.robbeverhelst.com",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	debounce, err := unifiDebounce()
	if err != nil {
		setupLog.Error(err, "Invalid debounce configuration")
		os.Exit(1)
	}
	store := engine.NewStateStore(engine.WithDebounce(unifi.ProviderName, debounce))
	// Providers push onto this when they observe a state change so the
	// affected Automations reconcile immediately; the periodic re-evaluation
	// in the reconciler is the backstop, not the mechanism.
	wake := make(chan event.GenericEvent, 256)

	// The UniFi provider is configured at the controller level (Helm values /
	// env), not per-Automation: one UniFi console per Reactor install.
	if unifiURL := os.Getenv("UNIFI_URL"); unifiURL != "" {
		apiKey, err := unifiAPIKey()
		if err != nil {
			setupLog.Error(err, "Failed to resolve the UniFi API key")
			os.Exit(1)
		}
		interval := 30 * time.Second
		if v := os.Getenv("UNIFI_POLL_INTERVAL"); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				setupLog.Error(err, "Invalid UNIFI_POLL_INTERVAL", "value", v)
				os.Exit(1)
			}
			interval = parsed
		}
		unifiClient := unifi.NewClient(unifiURL, apiKey, os.Getenv("UNIFI_SITE"),
			os.Getenv("UNIFI_INSECURE_SKIP_VERIFY") == "true")
		for _, threshold := range []struct {
			env    string
			target *int
		}{
			{"UNIFI_UPS_LOW_BATTERY_PERCENT", &unifiClient.LowBatteryPercent},
			{"UNIFI_UPS_CRITICAL_BATTERY_PERCENT", &unifiClient.CriticalBatteryPercent},
		} {
			v := os.Getenv(threshold.env)
			if v == "" {
				continue
			}
			parsed, err := strconv.Atoi(v)
			if err != nil {
				setupLog.Error(err, "Invalid battery threshold", "var", threshold.env, "value", v)
				os.Exit(1)
			}
			*threshold.target = parsed
		}
		poller := &controller.UniFiPoller{
			Client:   unifiClient,
			Store:    store,
			Interval: interval,
			Reader:   mgr.GetClient(),
			Events:   wake,
		}
		if err := mgr.Add(poller); err != nil {
			setupLog.Error(err, "Failed to add UniFi poller")
			os.Exit(1)
		}
		setupLog.Info("UniFi provider enabled", "url", unifiURL, "interval", interval)
	} else {
		setupLog.Info("UniFi provider disabled (UNIFI_URL not set); state triggers will stay pending")
	}

	if err := (&controller.AutomationReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Store:    store,
		Wake:     wake,
		Recorder: mgr.GetEventRecorder("automation"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "automation")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
