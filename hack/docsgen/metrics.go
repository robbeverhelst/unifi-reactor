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
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

// Every metric is one prometheus.NewXxx call taking an Opts literal, so the
// name, the help text and the label set are all readable from the syntax
// without building the package. What the syntax does not carry is why a metric
// exists and why its labels are the ones they are — that is in the doc comment
// above each var, and it is the half worth reading.

type metricDoc struct {
	varName string
	name    string
	kind    string
	help    string
	labels  []string
	buckets string
	doc     []string
}

func renderMetrics(dir string) (page, error) {
	files, err := parseDir(dir)
	if err != nil {
		return page{}, err
	}

	consts := stringConsts(files)
	var metrics []metricDoc
	var packageDoc []string
	for _, file := range files {
		if packageDoc == nil {
			packageDoc = docLines(file.Doc)
		}
		metrics = append(metrics, metricsInFile(file, consts)...)
	}
	if len(metrics) == 0 {
		return page{}, fmt.Errorf("%s: no metric definitions found", dir)
	}

	seriesNames := map[string]string{}
	for _, metric := range metrics {
		seriesNames[metric.varName] = metric.name
	}

	var b strings.Builder
	b.WriteString("Every series Reactor publishes, on the metrics endpoint the manager already serves. " +
		"Turn it on with `metrics.enabled`; " +
		"[Metrics, alerts and dashboard](/operations/metrics-and-alerts/) covers the auth posture and the " +
		"alert rules the chart ships.\n\n")

	b.WriteString("| Metric | Type | Labels |\n| --- | --- | --- |\n")
	for _, metric := range metrics {
		fmt.Fprintf(&b, "| [`%s`](#%s) | %s | %s |\n",
			metric.name, metric.name, metric.kind, labelList(metric.labels))
	}

	if len(packageDoc) > 0 {
		b.WriteString("\n## About these series\n\n")
		b.WriteString(substituteIdents(renderComment(packageDoc), seriesNames) + "\n")
	}

	for _, metric := range metrics {
		fmt.Fprintf(&b, "\n## `%s`\n\n", metric.name)
		fmt.Fprintf(&b, "**Type:** %s &nbsp;·&nbsp; **Labels:** %s\n", metric.kind, labelList(metric.labels))
		if metric.buckets != "" {
			fmt.Fprintf(&b, "\n**Buckets:** `%s`\n", metric.buckets)
		}
		if metric.help != "" {
			b.WriteString("\n" + escapeAngles(metric.help) + "\n")
		}
		if prose := renderComment(dropLeadingName(metric.doc, metric.varName)); prose != "" {
			b.WriteString("\n" + substituteIdents(prose, seriesNames) + "\n")
		}
	}

	return page{
		slug:        "metrics",
		title:       "Metrics",
		description: "Every Prometheus series Reactor publishes: its type, its labels, and why it exists.",
		source:      "internal/metrics/metrics.go",
		body:        b.String(),
	}, nil
}

func labelList(labels []string) string {
	if len(labels) == 0 {
		return "none"
	}
	quoted := make([]string, 0, len(labels))
	for _, label := range labels {
		quoted = append(quoted, "`"+label+"`")
	}
	return strings.Join(quoted, ", ")
}

func metricsInFile(file *ast.File, consts map[string]string) []metricDoc {
	var metrics []metricDoc
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.VAR {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != 1 || len(valueSpec.Values) != 1 {
				continue
			}
			call, ok := valueSpec.Values[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			metric, ok := metricFromCall(call, consts)
			if !ok {
				continue
			}
			metric.varName = valueSpec.Names[0].Name
			metric.doc = docLines(valueSpec.Doc)
			metrics = append(metrics, metric)
		}
	}
	return metrics
}

// metricFromCall reads one prometheus.NewXxx(XxxOpts{...}, []string{...}) call.
func metricFromCall(call *ast.CallExpr, consts map[string]string) (metricDoc, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return metricDoc{}, false
	}
	kind := metricKind(selector.Sel.Name)
	if kind == "" || len(call.Args) == 0 {
		return metricDoc{}, false
	}
	opts, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return metricDoc{}, false
	}

	metric := metricDoc{kind: kind}
	for _, elt := range opts.Elts {
		field, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Name":
			metric.name = stringValue(field.Value, consts)
		case "Help":
			metric.help = stringValue(field.Value, consts)
		case "Buckets":
			metric.buckets = literalList(field.Value)
		}
	}
	if metric.name == "" {
		return metricDoc{}, false
	}
	if len(call.Args) > 1 {
		metric.labels = stringSlice(call.Args[1], consts)
	}
	return metric, true
}

func metricKind(constructor string) string {
	if !strings.HasPrefix(constructor, "New") {
		return ""
	}
	switch strings.TrimSuffix(strings.TrimPrefix(constructor, "New"), "Vec") {
	case "Counter":
		return "counter"
	case "Gauge":
		return "gauge"
	case "Histogram":
		return "histogram"
	case "Summary":
		return "summary"
	default:
		return ""
	}
}

// literalList renders a []float64{...} literal the way it is written.
func literalList(expr ast.Expr) string {
	composite, ok := expr.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(composite.Elts))
	for _, elt := range composite.Elts {
		lit, ok := elt.(*ast.BasicLit)
		if !ok {
			return ""
		}
		parts = append(parts, lit.Value)
	}
	return strings.Join(parts, ", ")
}
