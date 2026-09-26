package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// operationsFromOpenAPI derives Operations from an OpenAPI 3.x JSON document
// (PRD FR-2). JSON only — convert YAML before importing. Each operation's
// input schema combines its path/query parameters and a JSON request body.

var nonIDChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func operationsFromOpenAPI(doc []byte) ([]Operation, error) {
	var spec struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(doc, &spec); err != nil {
		return nil, fmt.Errorf("openapi: not a JSON document: %w", err)
	}
	if !strings.HasPrefix(spec.OpenAPI, "3.") {
		return nil, fmt.Errorf("openapi: only OpenAPI 3.x JSON is supported (got %q)", spec.OpenAPI)
	}
	paths := make([]string, 0, len(spec.Paths))
	for p := range spec.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var ops []Operation
	seen := map[string]bool{}
	for _, p := range paths {
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			raw, ok := spec.Paths[p][method]
			if !ok {
				continue
			}
			var o struct {
				OperationID string `json:"operationId"`
				Summary     string `json:"summary"`
				Description string `json:"description"`
				Parameters  []struct {
					Name        string          `json:"name"`
					In          string          `json:"in"`
					Required    bool            `json:"required"`
					Description string          `json:"description"`
					Schema      json.RawMessage `json:"schema"`
				} `json:"parameters"`
				RequestBody struct {
					Required bool                                        `json:"required"`
					Content  map[string]struct{ Schema json.RawMessage } `json:"content"`
				} `json:"requestBody"`
			}
			if err := json.Unmarshal(raw, &o); err != nil {
				return nil, fmt.Errorf("openapi: %s %s: %w", method, p, err)
			}
			id := nonIDChars.ReplaceAllString(o.OperationID, "_")
			if id == "" || !opIDPattern.MatchString(id) {
				id = method + nonIDChars.ReplaceAllString(strings.ReplaceAll(p, "/", "_"), "")
			}
			if seen[id] {
				return nil, fmt.Errorf("openapi: duplicate operation id %q", id)
			}
			seen[id] = true

			props := map[string]json.RawMessage{}
			var required []string
			for _, prm := range o.Parameters {
				if prm.In != "path" && prm.In != "query" {
					continue
				}
				s := prm.Schema
				if len(s) == 0 {
					s = json.RawMessage(`{"type":"string"}`)
				}
				if prm.Description != "" {
					var m map[string]any
					if json.Unmarshal(s, &m) == nil {
						m["description"] = prm.Description
						s, _ = json.Marshal(m)
					}
				}
				props[prm.Name] = s
				if prm.Required || prm.In == "path" {
					required = append(required, prm.Name)
				}
			}
			op := Operation{ID: id, Method: strings.ToUpper(method), Path: p, Summary: o.Summary, Description: o.Description}
			if c, ok := o.RequestBody.Content["application/json"]; ok {
				body := c.Schema
				if len(body) == 0 {
					body = json.RawMessage(`{"type":"object"}`)
				}
				props["body"] = body
				op.BodyContentType = "application/json"
				if o.RequestBody.Required {
					required = append(required, "body")
				}
			}
			schema := map[string]any{"type": "object", "properties": props}
			if len(required) > 0 {
				schema["required"] = required
			}
			op.InputSchema, _ = json.Marshal(schema)
			ops = append(ops, op)
		}
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("openapi: no operations found")
	}
	return ops, nil
}
