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

// Package adopt hands a CustomResourceDefinition that belongs to no Helm
// release over to the release that now templates it.
//
// Chart 0.3.0 and earlier shipped the Automation CRD under crds/, which Helm
// applies on first install and never records as part of the release. Every
// chart since templates it, and Helm refuses to update an object it does not
// own — so the first upgrade stops with "invalid ownership metadata" and used
// to need a kubectl label and a kubectl annotate run by hand.
//
// This is that pair of commands, run by the chart's own hook. It is deliberately
// the narrowest thing that works: one CRD, named up front, adopted only when
// nothing else claims it, patched and never deleted or recreated.
package adopt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// releaseNameAnnotation and releaseNamespaceAnnotation are what Helm reads
	// to decide whether an object already in the cluster belongs to the release
	// it is about to apply, and managedByLabel is the label it checks alongside
	// them. All three have to be right, and "Helm" is the only value accepted.
	releaseNameAnnotation      = "meta.helm.sh/release-name"
	releaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
	managedByLabel             = "app.kubernetes.io/managed-by"
	managedByHelm              = "Helm"

	crdKind = "CustomResourceDefinition"
)

var crdGroupVersionKind = schema.GroupVersionKind{
	Group:   "apiextensions.k8s.io",
	Version: "v1",
	Kind:    crdKind,
}

// Options names what may be adopted and who is adopting it.
type Options struct {
	// Name is the one CustomResourceDefinition this may touch. Nothing here
	// searches for CRDs to adopt: the chart names the single one it owns, and
	// anything else in the cluster is somebody else's.
	Name string

	// Release and Namespace identify the release taking ownership. The chart
	// renders both from the release being installed, so a user who installs
	// under their own name and namespace adopts into that release rather than
	// into one the chart guessed at.
	Release   string
	Namespace string

	// ManifestPath is the chart's own copy of the CRD, mounted alongside this
	// hook. Its spec and its annotations — helm.sh/resource-policy: keep among
	// them — are applied in the same patch that takes ownership.
	//
	// It has to be, and that is the part worth explaining. Helm checks
	// ownership while it prepares the upgrade — before it runs a single hook —
	// so the chart leaves the CRD out of the release entirely on the upgrade
	// that adopts it, or the upgrade would fail before this ever ran. Nothing
	// else would then put the new schema live, and an operator expecting a
	// schema the API server does not have is the failure this whole packaging
	// exists to prevent.
	//
	// Empty adopts the ownership metadata alone, leaving the live schema as it
	// was.
	ManifestPath string
}

// CRD adopts the CustomResourceDefinition named in opts into the Helm release
// named there, and puts the chart's copy of the schema live with it.
//
// It succeeds without touching anything when there is nothing to adopt: no such
// CRD (a fresh install has nothing to take over) or one this release already
// owns (every upgrade after the first). It fails, naming what it found, when
// the CRD belongs to a different release — taking that would break the release
// that does own it, and no upgrade should quietly do that.
//
// Automations are never read or written here. Adoption is metadata on the CRD
// object and the schema it serves; the resources stored under it are untouched.
func CRD(ctx context.Context, c client.Client, opts Options) error {
	log := logf.FromContext(ctx)

	if opts.Name == "" || opts.Release == "" || opts.Namespace == "" {
		return errors.New("the CRD name, release name and release namespace are all required; " +
			"the chart renders them from the release being installed")
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(crdGroupVersionKind)
	if err := c.Get(ctx, client.ObjectKey{Name: opts.Name}, live); err != nil {
		if apierrors.IsNotFound(err) {
			// A fresh install, or one whose CRD is managed outside the release
			// and not applied yet. Either way the release installs its own.
			log.Info("No CustomResourceDefinition to adopt", "crd", opts.Name)
			return nil
		}
		return fmt.Errorf("reading the CustomResourceDefinition %q: %w", opts.Name, err)
	}

	annotations := live.GetAnnotations()
	switch owner, ownerNamespace := annotations[releaseNameAnnotation], annotations[releaseNamespaceAnnotation]; {
	case owner == "":
		// Owned by nobody, which is exactly what the crds/ packaging left
		// behind and the only state this adopts from.
	case owner == opts.Release && ownerNamespace == opts.Namespace:
		log.Info("CustomResourceDefinition is already part of this release",
			"crd", opts.Name, "release", opts.Release, "namespace", opts.Namespace)
		return nil
	default:
		return fmt.Errorf("the CustomResourceDefinition %q belongs to the Helm release %q in namespace %q, "+
			"not to %q in namespace %q; refusing to take it from that release. Install with crds.install=false "+
			"to leave the CRD to whoever owns it, or crds.adopt=false to hand it over yourself",
			opts.Name, owner, ownerNamespace, opts.Release, opts.Namespace)
	}

	patch, err := adoptionPatch(opts)
	if err != nil {
		return err
	}
	if err := c.Patch(ctx, live, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("adopting the CustomResourceDefinition %q into the release %q: %w",
			opts.Name, opts.Release, err)
	}

	log.Info("Adopted the CustomResourceDefinition into the release",
		"crd", opts.Name, "release", opts.Release, "namespace", opts.Namespace,
		"schemaApplied", opts.ManifestPath != "")
	return nil
}

// adoptionPatch builds the single merge patch that takes ownership and, when
// the chart's copy of the CRD was mounted, puts its schema and its annotations
// live.
//
// One patch rather than two: either the CRD ends up owned by the release and
// serving the schema that release ships, or nothing about it changed.
func adoptionPatch(opts Options) ([]byte, error) {
	annotations := map[string]any{}
	patch := map[string]any{"metadata": map[string]any{
		"labels":      map[string]any{managedByLabel: managedByHelm},
		"annotations": annotations,
	}}

	if opts.ManifestPath != "" {
		document, err := chartDocument(opts.ManifestPath, opts.Name)
		if err != nil {
			return nil, err
		}
		// A merge patch replaces spec.versions wholesale — it is a list, and
		// JSON merge semantics do not merge those — which is what makes this
		// the same schema change a normal `helm upgrade` applies.
		patch["spec"] = document["spec"]
		// The chart's annotations along with it, so an adopted CRD carries
		// helm.sh/resource-policy: keep from the moment it is adopted rather
		// than from the next upgrade, which is the first time Helm applies the
		// template itself. Annotations merge, so anything else on the live
		// object survives.
		maps.Copy(annotations, chartAnnotations(document))
	}

	// Last, so ownership is never something the chart's own manifest could
	// overwrite: it is the half of this patch that has to be exact.
	annotations[releaseNameAnnotation] = opts.Release
	annotations[releaseNamespaceAnnotation] = opts.Namespace
	return json.Marshal(patch)
}

// chartAnnotations reads metadata.annotations off a decoded document, without
// assuming either is present.
func chartAnnotations(document map[string]any) map[string]any {
	metadata, ok := document["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	annotations, _ := metadata["annotations"].(map[string]any)
	return annotations
}

// chartDocument reads the CRD the chart would have applied and returns it.
//
// The file is the chart's own rendered template, so this is strict about what
// it accepts: exactly one CustomResourceDefinition, carrying the name being
// adopted. Anything else means the hook was handed a manifest for a different
// object, and patching a CRD's schema from one of those is not a mistake worth
// recovering from.
func chartDocument(path, name string) (map[string]any, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the path is the chart's own mounted manifest
	if err != nil {
		return nil, fmt.Errorf("reading the chart's CRD at %s: %w", path, err)
	}

	var found map[string]any
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parsing the chart's CRD at %s: %w", path, err)
		}
		if len(document) == 0 {
			continue
		}
		if kind, _ := document["kind"].(string); kind != crdKind {
			return nil, fmt.Errorf("the chart's CRD at %s contains a %q, which this does not apply", path, kind)
		}
		if got := documentName(document); got != name {
			return nil, fmt.Errorf("the chart's CRD at %s is named %q, not the %q being adopted", path, got, name)
		}
		if found != nil {
			return nil, fmt.Errorf("the chart's CRD at %s holds more than one document", path)
		}
		if _, ok := document["spec"].(map[string]any); !ok {
			return nil, fmt.Errorf("the chart's CRD at %s has no spec", path)
		}
		found = document
	}
	if found == nil {
		return nil, fmt.Errorf("the chart's CRD at %s holds no CustomResourceDefinition", path)
	}
	return found, nil
}

// documentName reads metadata.name out of a decoded document, without assuming
// either is present.
func documentName(document map[string]any) string {
	metadata, ok := document["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := metadata["name"].(string)
	return name
}
