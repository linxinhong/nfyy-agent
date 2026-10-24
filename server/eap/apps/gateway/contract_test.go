package main

// Drift guard for api/mindlink-openapi.yaml (the TypeScript SDK source):
// every live operation must be a route server.go registers, and every
// operation marked `x-mindlink-status: planned` must not be registered yet,
// so the contract never overstates or understates what the gateway serves.

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	specPathLine   = regexp.MustCompile(`^  (/[^:]*):\s*$`)
	specMethodLine = regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)
	specPlanned    = regexp.MustCompile(`^      x-mindlink-status:\s*planned\s*$`)
	routeLiteral   = regexp.MustCompile(`HandleFunc\("([A-Z]+ [^"]+)"`)
)

func TestMindLinkContractRoutesAreRegistered(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, m := range routeLiteral.FindAllStringSubmatch(string(src), -1) {
		// OpenAPI has no rest-of-path wildcard: {path...} is documented as {path}.
		registered[strings.ReplaceAll(m[1], "...}", "}")] = true
	}
	f, err := os.Open("../../api/mindlink-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	inPaths := false
	path := ""
	var operations []string
	planned := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "paths:" {
			inPaths = true
			continue
		}
		if !inPaths {
			continue
		}
		if m := specPathLine.FindStringSubmatch(line); m != nil {
			path = m[1]
			continue
		}
		if m := specMethodLine.FindStringSubmatch(line); m != nil && path != "" {
			operations = append(operations, strings.ToUpper(m[1])+" "+path)
			continue
		}
		if specPlanned.MatchString(line) && len(operations) > 0 {
			planned[operations[len(operations)-1]] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(operations) < 30 {
		t.Fatalf("parsed only %d operations — the contract layout no longer matches this guard", len(operations))
	}
	for _, route := range operations {
		switch {
		case planned[route] && registered[route]:
			t.Errorf("%s is registered but still marked planned in the contract", route)
		case !planned[route] && !registered[route]:
			t.Errorf("contract operation %s is not registered in server.go", route)
		}
	}
}
