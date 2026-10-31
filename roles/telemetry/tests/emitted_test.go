package tests_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// telemetrySource is Foreman's telemetry package source. The role module cannot import Foreman's
// internal packages, so the specs parse the file that declares the attribute names instead.
const telemetrySource = "../../../foreman/internal/telemetry/telemetry.go"

// semconvKeys are the pinned GenAI convention keys Foreman's attributes take from the semconv
// package, which a parse of this file alone cannot resolve. An attribute built from a key not
// listed here is left out of the emitted names, so an alert selecting on it fails.
var semconvKeys = map[string]string{
	"GenAIRequestModelKey": "gen_ai.request.model",
}

// emittedLabels parses the Go source and returns the Prometheus label names of its Attr* string
// constants: the attribute name with its dots turned to underscores, as the OTLP receiver does.
func emittedLabels(path string) (map[string]bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	labels := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Attr") || i >= len(vs.Values) {
					continue
				}
				if attr, ok := attributeName(vs.Values[i]); ok {
					labels[strings.ReplaceAll(attr, ".", "_")] = true
				}
			}
		}
	}
	return labels, nil
}

// attributeName is the attribute a constant's value names: a string literal, or string(semconv.Key).
func attributeName(value ast.Expr) (string, bool) {
	switch v := value.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.CallExpr:
		if len(v.Args) != 1 {
			return "", false
		}
		sel, ok := v.Args[0].(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		s, ok := semconvKeys[sel.Sel.Name]
		return s, ok
	default:
		return "", false
	}
}
