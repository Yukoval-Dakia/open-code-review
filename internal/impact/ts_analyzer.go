// internal/impact/ts_analyzer.go
package impact

import (
	_ "embed"
	"encoding/json"
	"os/exec"
	"strings"
)

//go:embed ts_refs.js
var tsRefsScript []byte

type tsAnalyzer struct{}

func (tsAnalyzer) Supports(path string) bool {
	return strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx")
}

type tsRequest struct {
	Mode    string `json:"mode"`
	Content string `json:"content"`
	Changed []int  `json:"changed,omitempty"`
	Name    string `json:"name,omitempty"`
}

type tsResponse struct {
	Symbols []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		Line int    `json:"line"`
	} `json:"symbols"`
	Refs []struct {
		Line int    `json:"line"`
		Kind string `json:"kind"`
	} `json:"refs"`
	Error string `json:"error"`
}

func runTSHelper(req tsRequest) (tsResponse, error) {
	var resp tsResponse
	in, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	cmd := exec.Command("node", "-e", string(tsRefsScript))
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.Output()
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return resp, err
	}
	if resp.Error != "" {
		return resp, &helperError{resp.Error}
	}
	return resp, nil
}

type helperError struct{ msg string }

func (e *helperError) Error() string { return "ts helper: " + e.msg }

// nodeHasTypeScript reports whether node can require('typescript') from CWD.
func nodeHasTypeScript() bool {
	cmd := exec.Command("node", "-e", "require.resolve('typescript')")
	return cmd.Run() == nil
}

func (tsAnalyzer) ChangedSymbols(content string, changed map[int]bool) ([]Symbol, error) {
	lines := make([]int, 0, len(changed))
	for ln := range changed {
		lines = append(lines, ln)
	}
	resp, err := runTSHelper(tsRequest{Mode: "symbols", Content: content, Changed: lines})
	if err != nil {
		return nil, err
	}
	out := make([]Symbol, 0, len(resp.Symbols))
	for _, s := range resp.Symbols {
		out = append(out, Symbol{Name: s.Name, Kind: s.Kind, DefLine: s.Line})
	}
	return out, nil
}

func (tsAnalyzer) References(path, content, name string) ([]Reference, error) {
	resp, err := runTSHelper(tsRequest{Mode: "refs", Content: content, Name: name})
	if err != nil {
		return nil, err
	}
	out := make([]Reference, 0, len(resp.Refs))
	for _, r := range resp.Refs {
		out = append(out, Reference{File: path, Line: r.Line, Kind: r.Kind})
	}
	return out, nil
}
