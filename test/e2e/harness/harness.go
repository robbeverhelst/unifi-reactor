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

// Package harness drives a throwaway Kind cluster for the e2e suites.
//
// Every kubectl and helm invocation goes through here so that every one of
// them names its cluster explicitly. An unpinned command inherits whatever
// context happens to be current, and these suites install an operator with
// cluster-wide RBAC, uninstall releases, and delete CRDs — on the wrong
// cluster that is not a failed test, it is an outage. Cluster refuses to
// address anything that is not a local Kind context.
package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// kindContextPrefix is the context name Kind gives every cluster it creates.
// Refusing anything else is the guard that keeps these suites off a real
// cluster even if KIND_CLUSTER is set to something unexpected.
const kindContextPrefix = "kind-"

// Cluster is a Kind cluster addressed by an explicit context.
type Cluster struct {
	// Name is the Kind cluster name, as `kind create cluster --name` took it.
	Name string
	// Context is the kubeconfig context, always passed to every command.
	Context string
	// Log receives every command line and its output.
	Log io.Writer
}

// NewCluster resolves the cluster the suite may act on from KIND_CLUSTER and
// verifies it is reachable before any test touches it.
func NewCluster(log io.Writer) (*Cluster, error) {
	name := os.Getenv("KIND_CLUSTER")
	if name == "" {
		return nil, fmt.Errorf("KIND_CLUSTER is not set; run these suites through the Makefile, " +
			"which creates a throwaway Kind cluster and names it")
	}
	if strings.HasPrefix(name, kindContextPrefix) {
		// Being handed "kind-foo" instead of "foo" would otherwise silently
		// address a context named "kind-kind-foo" that does not exist.
		return nil, fmt.Errorf("KIND_CLUSTER=%q looks like a context name; it must be the Kind cluster name", name)
	}
	cluster := &Cluster{Name: name, Context: kindContextPrefix + name, Log: log}
	if _, err := cluster.Kubectl("cluster-info"); err != nil {
		return nil, fmt.Errorf("kind cluster %q is not reachable: %w", name, err)
	}
	return cluster, nil
}

// Quiet returns the same cluster with command logging discarded, for polling
// loops whose output would otherwise bury the test's own narrative.
func (c *Cluster) Quiet() *Cluster {
	quiet := *c
	quiet.Log = io.Discard
	return &quiet
}

// Kubectl runs kubectl against this cluster and nothing else.
func (c *Cluster) Kubectl(args ...string) (string, error) {
	return Run(c.Log, ProjectDir(), "kubectl", append([]string{"--context", c.Context}, args...)...)
}

// KubectlInput runs kubectl with stdin, for `apply -f -`.
func (c *Cluster) KubectlInput(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--context", c.Context}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	return run(c.Log, ProjectDir(), cmd)
}

// Helm runs helm against this cluster and nothing else.
func (c *Cluster) Helm(args ...string) (string, error) {
	return Run(c.Log, ProjectDir(), "helm", append([]string{"--kube-context", c.Context}, args...)...)
}

// Apply applies a manifest given as text.
func (c *Cluster) Apply(manifest string) error {
	_, err := c.KubectlInput(manifest, "apply", "-f", "-")
	return err
}

// Delete removes a manifest given as text, tolerating what is already gone.
func (c *Cluster) Delete(manifest string) error {
	_, err := c.KubectlInput(manifest, "delete", "--ignore-not-found", "-f", "-")
	return err
}

// GetInto reads one object into a typed value, so assertions are written
// against the real API types rather than against jsonpath strings.
func (c *Cluster) GetInto(object any, kind, namespace, name string) error {
	out, err := c.Kubectl("get", kind, name, "-n", namespace, "-o", "json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), object); err != nil {
		return fmt.Errorf("decoding %s/%s in %s: %w", kind, name, namespace, err)
	}
	return nil
}

// LoadImage makes a locally built image available to the cluster's nodes.
func (c *Cluster) LoadImage(image string) error {
	kind := "kind"
	if v := os.Getenv("KIND"); v != "" {
		kind = v
	}
	_, err := Run(c.Log, ProjectDir(), kind, "load", "docker-image", image, "--name", c.Name)
	return err
}

// Run executes a command in dir and returns its combined output.
func Run(log io.Writer, dir, name string, args ...string) (string, error) {
	return run(log, dir, exec.Command(name, args...)) // #nosec G204 -- test harness, arguments are literals
}

// RunEnv is Run with extra environment entries, for the one caller that
// cross-compiles.
func RunEnv(log io.Writer, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) // #nosec G204 -- test harness, arguments are literals
	cmd.Env = append(os.Environ(), env...)
	return run(log, dir, cmd)
}

func run(log io.Writer, dir string, cmd *exec.Cmd) (string, error) {
	cmd.Dir = dir
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	_, _ = fmt.Fprintf(log, "running: %s\n", strings.Join(cmd.Args, " "))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s failed: %w\n%s", strings.Join(cmd.Args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// ProjectDir is the repository root, found by walking up to the go.mod rather
// than by editing the working directory's path, so it holds wherever a suite
// lives.
func ProjectDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

// BuildManagerImage builds the operator image exactly as a release would.
func BuildManagerImage(log io.Writer, image string) error {
	_, err := Run(log, ProjectDir(), "docker", "build", "-t", image, ".")
	return err
}
