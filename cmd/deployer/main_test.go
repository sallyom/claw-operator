/*
Copyright 2026 Red Hat.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestValidateNamespace(t *testing.T) {
	for _, namespace := range []string{"sallyom-claw", "user123", "a"} {
		if err := validateNamespace(namespace); err != nil {
			t.Fatalf("expected %q to be valid: %v", namespace, err)
		}
	}
	for _, namespace := range []string{"", "Upper", "-bad", "bad-", "bad_namespace"} {
		if err := validateNamespace(namespace); err == nil {
			t.Fatalf("expected %q to be invalid", namespace)
		}
	}
}

func TestKubeJSONImpersonatesUser(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://kubernetes.example.test/api/v1/namespaces/sallyom-claw" {
				t.Fatalf("URL = %q", r.URL.String())
			}
			if got := r.Header.Get("Authorization"); got != "Bearer service-account-token" {
				t.Fatalf("Authorization = %q", got)
			}
			if got := r.Header.Get("Impersonate-User"); got != "sallyom" {
				t.Fatalf("Impersonate-User = %q", got)
			}
			groups := r.Header.Values("Impersonate-Group")
			if len(groups) != 2 || groups[0] != "system:authenticated" || groups[1] != "team-a" {
				t.Fatalf("Impersonate-Group = %#v", groups)
			}
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}),
	}

	s := &server{
		apiServer:   "https://kubernetes.example.test",
		bearerToken: "service-account-token",
		impersonate: true,
		client:      client,
	}
	identity := userIdentity{Name: "sallyom", Groups: []string{"system:authenticated", "team-a"}}
	if err := s.kubeJSON(context.Background(), identity, http.MethodGet, "/api/v1/namespaces/sallyom-claw", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentIdentity(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-User", "sallyom")
	req.Header.Set("X-Forwarded-Groups", "team-a, team-b")
	identity, err := currentIdentity(req)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Name != "sallyom" {
		t.Fatalf("expected forwarded user, got %q", identity.Name)
	}
	expectedGroups := []string{"team-a", "team-b", "system:authenticated", "system:authenticated:oauth"}
	if len(identity.Groups) != len(expectedGroups) {
		t.Fatalf("expected groups %#v, got %#v", expectedGroups, identity.Groups)
	}
	for i, expected := range expectedGroups {
		if identity.Groups[i] != expected {
			t.Fatalf("expected group %d to be %q, got %q", i, expected, identity.Groups[i])
		}
	}
}

func TestCurrentUser(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-User", "sallyom")
	user, err := currentUser(req)
	if err != nil {
		t.Fatal(err)
	}
	if user != "sallyom" {
		t.Fatalf("expected forwarded user, got %q", user)
	}
}

func TestAllowedNamespaceForUser(t *testing.T) {
	tests := map[string]string{
		"sallyom":             "sallyom-claw",
		"octo-claw":           "octo-claw",
		"Sally.OM@example.io": "sally-om-claw",
	}
	for username, expected := range tests {
		if actual := allowedNamespaceForUser(username, defaultNSSuffix); actual != expected {
			t.Fatalf("allowedNamespaceForUser(%q) = %q, want %q", username, actual, expected)
		}
	}
}

func TestUpsertCredentialAppendsAndReplacesProvider(t *testing.T) {
	credentials := []any{
		map[string]any{
			"name":     "openai",
			"provider": "openai",
			"secretRef": []any{
				map[string]any{"name": "openclaw-instance-openai-api-key", "key": apiKeySecretKey},
			},
		},
	}
	credentials = upsertCredential(credentials, "instance", "openrouter")
	if len(credentials) != 2 {
		t.Fatalf("expected two credentials, got %#v", credentials)
	}
	credentials = upsertCredential(credentials, "instance", "openai")
	if len(credentials) != 2 {
		t.Fatalf("expected replacement without duplicate, got %#v", credentials)
	}
	first := credentials[0].(map[string]any)
	if first["provider"] != "openai" {
		t.Fatalf("expected first provider to stay openai, got %#v", first)
	}
}

func TestNormalizeModelRef(t *testing.T) {
	tests := map[string]struct {
		provider string
		model    string
		want     string
	}{
		"openrouter nested": {provider: "openrouter", model: "anthropic/claude-sonnet-4-6", want: "openrouter/anthropic/claude-sonnet-4-6"},
		"openrouter full":   {provider: "openrouter", model: "openrouter/auto", want: "openrouter/auto"},
		"anthropic bare":    {provider: "anthropic", model: "claude-sonnet-4-6", want: "anthropic/claude-sonnet-4-6"},
		"google empty":      {provider: "google", model: "", want: "google/gemini-3.1-pro-preview"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := normalizeModelRef(tt.provider, tt.model); got != tt.want {
				t.Fatalf("normalizeModelRef(%q, %q) = %q, want %q", tt.provider, tt.model, got, tt.want)
			}
		})
	}
}

func TestApplyAgentConfig(t *testing.T) {
	raw := applyAgentConfig(map[string]any{}, "SallyBot", "openrouter/anthropic/claude-sonnet-4-6")
	primary, _, _ := nestedString(raw, "agents", "defaults", "model", "primary")
	if primary != "openrouter/anthropic/claude-sonnet-4-6" {
		t.Fatalf("primary = %q", primary)
	}
	agents, _, _ := nestedSlice(raw, "agents", "list")
	first := agents[0].(map[string]any)
	if first["name"] != "SallyBot" {
		t.Fatalf("agent name = %#v", first["name"])
	}
}

func TestReadyCondition(t *testing.T) {
	claw := map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{
					"type":    "Ready",
					"status":  "True",
					"reason":  "Ready",
					"message": "Claw instance is ready",
				},
			},
		},
	}
	ready, reason, message := readyCondition(claw)
	if !ready || reason != "Ready" || message == "" {
		t.Fatalf("unexpected condition: ready=%v reason=%q message=%q", ready, reason, message)
	}
}
