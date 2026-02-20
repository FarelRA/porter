package main

import (
	"strings"
	"testing"
)

func TestIsValidRuleID(t *testing.T) {
	valid := []string{"deadbeef", "rule-1", "abc_DEF.123"}
	invalid := []string{"", " ", "a/b", "a\\b", "a?b", strings.Repeat("a", 65)}

	for _, id := range valid {
		if !isValidRuleID(id) {
			t.Fatalf("expected valid id %q", id)
		}
	}
	for _, id := range invalid {
		if isValidRuleID(id) {
			t.Fatalf("expected invalid id %q", id)
		}
	}
}

func TestNormalizeRuleDefaults(t *testing.T) {
	r, err := normalizeRule(Rule{ListenPort: 1234, TargetType: "host", TargetHost: "127.0.0.1", TargetPort: 80})
	if err != nil {
		t.Fatalf("normalizeRule error: %v", err)
	}
	if r.Protocol != "tcp" {
		t.Fatalf("expected protocol tcp, got %q", r.Protocol)
	}
	if r.ListenHost != "0.0.0.0" {
		t.Fatalf("expected default listen host 0.0.0.0, got %q", r.ListenHost)
	}
	if r.ID == "" {
		t.Fatalf("expected generated id")
	}
}

func TestRulesEqualForRunner(t *testing.T) {
	base := Rule{
		ID:              "deadbeef",
		Name:            "A",
		Enabled:         true,
		Protocol:        "tcp",
		ListenHost:      "127.0.0.1",
		ListenPort:      1111,
		TargetType:      "host",
		TargetHost:      "127.0.0.1",
		TargetPort:      2222,
		TargetContainer: "",
		TargetNetwork:   "",
	}

	changedName := base
	changedName.Name = "B"
	changedName.Enabled = false
	if !rulesEqualForRunner(base, changedName) {
		t.Fatalf("name/enabled changes should not require restart")
	}

	changedPort := base
	changedPort.ListenPort = 3333
	if rulesEqualForRunner(base, changedPort) {
		t.Fatalf("listen port change should require restart")
	}
}
