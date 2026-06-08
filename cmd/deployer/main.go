/*
Copyright 2026 Red Hat.

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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed static/*
var staticFiles embed.FS

const (
	apiKeySecretKey    = "api-key"
	fieldManager       = "openclaw-deployer"
	managedByLabel     = "app.kubernetes.io/managed-by"
	managedByValue     = "openclaw-deployer"
	instanceLabel      = "openclaw-deployer.redhat.com/instance"
	providerLabel      = "openclaw-deployer.redhat.com/provider"
	inClusterCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	inClusterNSPath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	inClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultListenAddr  = ":8080"
	defaultNSSuffix    = "-claw"
)

var (
	namespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsCharRE   = regexp.MustCompile(`[^a-z0-9-]+`)
	providers   = map[string]struct{}{
		"anthropic":  {},
		"google":     {},
		"openai":     {},
		"openrouter": {},
		"xai":        {},
	}
)

type server struct {
	apiServer       string
	bearerToken     string
	impersonate     bool
	namespaceSuffix string
	client          *http.Client
	static          http.Handler
}

type userIdentity struct {
	Name   string
	Groups []string
}

type provisionRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	AgentName string `json:"agentName"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	APIKey    string `json:"apiKey"`
}

type meResponse struct {
	User             string   `json:"user,omitempty"`
	DefaultNamespace string   `json:"defaultNamespace,omitempty"`
	Providers        []string `json:"providers"`
}

type stateResponse struct {
	Name       string   `json:"name,omitempty"`
	Exists     bool     `json:"exists"`
	Ready      bool     `json:"ready"`
	Reason     string   `json:"reason,omitempty"`
	Message    string   `json:"message,omitempty"`
	GatewayURL string   `json:"gatewayURL,omitempty"`
	Provider   string   `json:"provider,omitempty"`
	Providers  []string `json:"providers,omitempty"`
	Model      string   `json:"model,omitempty"`
	AgentName  string   `json:"agentName,omitempty"`
	CreatedAt  string   `json:"createdAt,omitempty"`
}

type listResponse struct {
	Claws []stateResponse `json:"claws"`
}

func main() {
	s, err := newServer()
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/claws", s.handleClaws)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/provision", s.handleProvision)
	mux.HandleFunc("POST /api/restart", s.handleRestart)
	mux.HandleFunc("DELETE /api/claw", s.handleDelete)
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /", s.handleIndex)

	addr := getenv("LISTEN_ADDR", defaultListenAddr)
	log.Printf("openclaw deployer listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func newServer() (*server, error) {
	apiServer, err := kubeAPIServerURL()
	if err != nil {
		return nil, err
	}

	client, err := kubeHTTPClient()
	if err != nil {
		return nil, err
	}
	bearerToken, impersonate, err := kubeBearerToken()
	if err != nil {
		return nil, err
	}

	return &server{
		apiServer:       apiServer,
		bearerToken:     bearerToken,
		impersonate:     impersonate,
		namespaceSuffix: getenv("CLAW_NAMESPACE_SUFFIX", defaultNSSuffix),
		client:          client,
		static:          http.FileServer(http.FS(staticFiles)),
	}, nil
}

func kubeAPIServerURL() (string, error) {
	if override := os.Getenv("KUBE_API_SERVER"); override != "" {
		return strings.TrimRight(override, "/"), nil
	}
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := getenv("KUBERNETES_SERVICE_PORT", "443")
	if host == "" {
		return "", errors.New("KUBERNETES_SERVICE_HOST is not set; set KUBE_API_SERVER for local testing")
	}
	return "https://" + host + ":" + port, nil
}

func kubeHTTPClient() (*http.Client, error) {
	caPEM, err := os.ReadFile(inClusterCAPath)
	if err != nil {
		if os.Getenv("KUBE_API_SERVER") != "" {
			return &http.Client{Timeout: 20 * time.Second}, nil
		}
		return nil, fmt.Errorf("read Kubernetes CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("failed to parse Kubernetes CA bundle")
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}, nil
}

func kubeBearerToken() (string, bool, error) {
	if token := strings.TrimSpace(os.Getenv("DEVELOPER_BEARER_TOKEN")); token != "" {
		return token, strings.EqualFold(os.Getenv("OPENCLAW_DEPLOYER_IMPERSONATE"), "true"), nil
	}
	token, err := os.ReadFile(inClusterTokenPath)
	if err != nil {
		return "", false, fmt.Errorf("read Kubernetes service account token: %w", err)
	}
	return strings.TrimSpace(string(token)), true, nil
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := currentUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, meResponse{
		User:             user,
		DefaultNamespace: allowedNamespaceForUser(user, s.namespaceSuffix),
		Providers:        []string{"openrouter", "openai", "google", "anthropic", "xai"},
	})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, staticFiles, "static/index.html")
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	identity, err := currentIdentity(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	namespace := r.URL.Query().Get("namespace")
	if err := validateNamespace(namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.authorizeNamespace(identity.Name, namespace); err != nil {
		writeError(w, statusCodeFor(err), err.Error())
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		name = "instance"
	}
	if err := validateResourceName(name, "Claw name"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	state, err := s.getState(r.Context(), identity, namespace, name)
	if err != nil {
		var apiErr apiError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			writeJSON(w, http.StatusOK, stateResponse{Name: name, Exists: false})
			return
		}
		writeError(w, statusCodeFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *server) handleClaws(w http.ResponseWriter, r *http.Request) {
	identity, err := currentIdentity(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	namespace := r.URL.Query().Get("namespace")
	if err := validateNamespace(namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.authorizeNamespace(identity.Name, namespace); err != nil {
		writeError(w, statusCodeFor(err), err.Error())
		return
	}

	claws, err := s.listClaws(r.Context(), identity, namespace)
	if err != nil {
		var apiErr apiError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusForbidden) {
			writeJSON(w, http.StatusOK, listResponse{Claws: []stateResponse{}})
			return
		}
		writeError(w, statusCodeFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listResponse{Claws: claws})
}

func (s *server) handleProvision(w http.ResponseWriter, r *http.Request) {
	identity, err := currentIdentity(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req provisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = "instance"
	}
	req.AgentName = strings.TrimSpace(req.AgentName)
	if req.AgentName == "" {
		req.AgentName = "OpenClaw Assistant"
	}
	req.Model = normalizeModelRef(req.Provider, req.Model)
	req.Namespace = strings.TrimSpace(req.Namespace)
	if err := validateNamespace(req.Namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateResourceName(req.Name, "Claw name"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.AgentName) > 80 {
		writeError(w, http.StatusBadRequest, "Agent name must be 80 characters or fewer")
		return
	}
	if err := s.authorizeNamespace(identity.Name, req.Namespace); err != nil {
		writeError(w, statusCodeFor(err), err.Error())
		return
	}
	if _, ok := providers[req.Provider]; !ok {
		writeError(w, http.StatusBadRequest, "unsupported provider")
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		writeError(w, http.StatusBadRequest, "API key is required")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "Model is required")
		return
	}

	if err := s.ensureProject(r.Context(), identity, req.Namespace); err != nil {
		writeError(w, statusCodeFor(err), "failed to create project: "+err.Error())
		return
	}
	if err := s.applySecret(r.Context(), identity, req); err != nil {
		writeError(w, statusCodeFor(err), "failed to create provider secret: "+err.Error())
		return
	}
	if err := s.applyClaw(r.Context(), identity, req); err != nil {
		writeError(w, statusCodeFor(err), "failed to create Claw: "+err.Error())
		return
	}

	state, err := s.getState(r.Context(), identity, req.Namespace, req.Name)
	if err != nil {
		writeError(w, statusCodeFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *server) handleRestart(w http.ResponseWriter, r *http.Request) {
	identity, err := currentIdentity(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	namespace := r.URL.Query().Get("namespace")
	if err := validateNamespace(namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.authorizeNamespace(identity.Name, namespace); err != nil {
		writeError(w, statusCodeFor(err), err.Error())
		return
	}
	name := r.URL.Query().Get("name")
	if err := validateResourceName(name, "Claw name"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.restartDeployments(r.Context(), identity, namespace, name); err != nil {
		writeError(w, statusCodeFor(err), "failed to restart OpenClaw: "+err.Error())
		return
	}
	state, err := s.getState(r.Context(), identity, namespace, name)
	if err != nil {
		writeError(w, statusCodeFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	identity, err := currentIdentity(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	namespace := r.URL.Query().Get("namespace")
	if err := validateNamespace(namespace); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.authorizeNamespace(identity.Name, namespace); err != nil {
		writeError(w, statusCodeFor(err), err.Error())
		return
	}
	name := r.URL.Query().Get("name")
	if err := validateResourceName(name, "Claw name"); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	state, _ := s.getState(r.Context(), identity, namespace, name)
	if err := s.delete(r.Context(), identity, apiPath("apis/claw.sandbox.redhat.com/v1alpha1/namespaces", namespace, "claws", name)); err != nil {
		writeError(w, statusCodeFor(err), "failed to delete Claw: "+err.Error())
		return
	}
	if state.Provider != "" {
		_ = s.deleteManagedSecret(r.Context(), identity, namespace, name, state.Provider)
	}
	writeJSON(w, http.StatusOK, stateResponse{Exists: false})
}

func (s *server) listClaws(ctx context.Context, identity userIdentity, namespace string) ([]stateResponse, error) {
	var list map[string]any
	if err := s.kubeJSON(ctx, identity, http.MethodGet, apiPath("apis/claw.sandbox.redhat.com/v1alpha1/namespaces", namespace, "claws"), nil, &list); err != nil {
		return nil, err
	}
	items, _, _ := nestedSlice(list, "items")
	claws := make([]stateResponse, 0, len(items))
	for _, item := range items {
		claw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		claws = append(claws, stateFromClaw(claw))
	}
	sort.Slice(claws, func(i, j int) bool {
		return claws[i].Name < claws[j].Name
	})
	return claws, nil
}

func (s *server) getState(ctx context.Context, identity userIdentity, namespace, name string) (stateResponse, error) {
	var claw map[string]any
	err := s.kubeJSON(ctx, identity, http.MethodGet, apiPath("apis/claw.sandbox.redhat.com/v1alpha1/namespaces", namespace, "claws", name), nil, &claw)
	if err != nil {
		return stateResponse{}, err
	}
	return stateFromClaw(claw), nil
}

func stateFromClaw(claw map[string]any) stateResponse {
	ready, reason, message := readyCondition(claw)
	gatewayURL, _, _ := nestedString(claw, "status", "gatewayURL")
	if gatewayURL == "" {
		gatewayURL, _, _ = nestedString(claw, "status", "url")
	}
	providers := credentialProviders(claw)
	provider := ""
	if len(providers) > 0 {
		provider = providers[0]
	}
	name, _, _ := nestedString(claw, "metadata", "name")
	createdAt, _, _ := nestedString(claw, "metadata", "creationTimestamp")
	model, _, _ := nestedString(claw, "spec", "config", "raw", "agents", "defaults", "model", "primary")
	agentName := firstAgentName(claw)

	return stateResponse{
		Name:       name,
		Exists:     true,
		Ready:      ready,
		Reason:     reason,
		Message:    message,
		GatewayURL: gatewayURL,
		Provider:   provider,
		Providers:  providers,
		Model:      model,
		AgentName:  agentName,
		CreatedAt:  createdAt,
	}
}

func (s *server) applySecret(ctx context.Context, identity userIdentity, req provisionRequest) error {
	body := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      secretName(req.Name, req.Provider),
			"namespace": req.Namespace,
			"labels": map[string]string{
				managedByLabel: managedByValue,
				instanceLabel:  req.Name,
				providerLabel:  req.Provider,
			},
		},
		"type": "Opaque",
		"data": map[string]string{
			apiKeySecretKey: base64.StdEncoding.EncodeToString([]byte(req.APIKey)),
		},
	}
	return s.apply(ctx, identity, apiPath("api/v1/namespaces", req.Namespace, "secrets", secretName(req.Name, req.Provider)), body)
}

func (s *server) ensureProject(ctx context.Context, identity userIdentity, namespace string) error {
	if err := s.kubeJSON(ctx, identity, http.MethodGet, apiPath("api/v1/namespaces", namespace), nil, nil); err == nil {
		return nil
	} else {
		var apiErr apiError
		if !errors.As(err, &apiErr) || (apiErr.StatusCode != http.StatusNotFound && apiErr.StatusCode != http.StatusForbidden) {
			return err
		}
	}

	body := map[string]any{
		"apiVersion": "project.openshift.io/v1",
		"kind":       "ProjectRequest",
		"metadata": map[string]string{
			"name": namespace,
		},
	}
	err := s.kubeJSON(ctx, identity, http.MethodPost, "/apis/project.openshift.io/v1/projectrequests", body, nil)
	var apiErr apiError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		return nil
	}
	return err
}

func (s *server) applyClaw(ctx context.Context, identity userIdentity, req provisionRequest) error {
	credentials, rawConfig := s.currentClawSpec(ctx, identity, req.Namespace, req.Name)
	credentials = upsertCredential(credentials, req.Name, req.Provider)
	rawConfig = applyAgentConfig(rawConfig, req.AgentName, req.Model)

	body := map[string]any{
		"apiVersion": "claw.sandbox.redhat.com/v1alpha1",
		"kind":       "Claw",
		"metadata": map[string]any{
			"name":      req.Name,
			"namespace": req.Namespace,
			"labels": map[string]string{
				managedByLabel: managedByValue,
			},
		},
		"spec": map[string]any{
			"credentials": credentials,
			"config": map[string]any{
				"raw":       rawConfig,
				"mergeMode": "merge",
			},
		},
	}
	return s.apply(ctx, identity, apiPath("apis/claw.sandbox.redhat.com/v1alpha1/namespaces", req.Namespace, "claws", req.Name), body)
}

func (s *server) currentClawSpec(ctx context.Context, identity userIdentity, namespace, name string) ([]any, map[string]any) {
	var claw map[string]any
	err := s.kubeJSON(ctx, identity, http.MethodGet, apiPath("apis/claw.sandbox.redhat.com/v1alpha1/namespaces", namespace, "claws", name), nil, &claw)
	if err != nil {
		return nil, map[string]any{}
	}
	credentials, _, _ := nestedSlice(claw, "spec", "credentials")
	raw, _, _ := nestedMap(claw, "spec", "config", "raw")
	return credentials, cloneMap(raw)
}

func upsertCredential(credentials []any, instanceName, provider string) []any {
	next := make([]any, 0, len(credentials)+1)
	replaced := false
	for _, credential := range credentials {
		credentialMap, ok := credential.(map[string]any)
		if !ok {
			continue
		}
		name, _ := credentialMap["name"].(string)
		if name == provider {
			next = append(next, providerCredential(instanceName, provider))
			replaced = true
			continue
		}
		next = append(next, credentialMap)
	}
	if !replaced {
		next = append(next, providerCredential(instanceName, provider))
	}
	return next
}

func providerCredential(instanceName, provider string) map[string]any {
	return map[string]any{
		"name":     provider,
		"provider": provider,
		"secretRef": []map[string]string{
			{"name": secretName(instanceName, provider), "key": apiKeySecretKey},
		},
	}
}

func applyAgentConfig(raw map[string]any, agentName, model string) map[string]any {
	config := cloneMap(raw)
	agents := ensureMap(config, "agents")
	defaults := ensureMap(agents, "defaults")
	defaults["model"] = map[string]any{"primary": model}
	models := ensureMap(defaults, "models")
	models[model] = map[string]any{"alias": model}
	agents["list"] = []any{
		map[string]any{
			"id":        "default",
			"name":      agentName,
			"identity":  map[string]string{"name": agentName},
			"workspace": "~/.openclaw/workspace",
			"model":     map[string]any{"primary": model},
		},
	}
	return config
}

func (s *server) restartDeployments(ctx context.Context, identity userIdentity, namespace, name string) error {
	restartedAt := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						"openclaw-deployer.redhat.com/restartedAt": restartedAt,
					},
				},
			},
		},
	}
	for _, deployment := range []string{name, name + "-proxy"} {
		if err := s.mergePatch(ctx, identity, apiPath("apis/apps/v1/namespaces", namespace, "deployments", deployment), patch); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) apply(ctx context.Context, identity userIdentity, path string, body any) error {
	return s.kubeJSON(ctx, identity, http.MethodPatch, path+"?fieldManager="+url.QueryEscape(fieldManager)+"&force=true", body, nil)
}

func (s *server) mergePatch(ctx context.Context, identity userIdentity, path string, body any) error {
	return s.kubeJSONWithContentType(ctx, identity, http.MethodPatch, path, body, nil, "application/merge-patch+json")
}

func (s *server) delete(ctx context.Context, identity userIdentity, path string) error {
	err := s.kubeJSON(ctx, identity, http.MethodDelete, path, nil, nil)
	var apiErr apiError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (s *server) deleteManagedSecret(ctx context.Context, identity userIdentity, namespace, name, provider string) error {
	var secret map[string]any
	secretPath := apiPath("api/v1/namespaces", namespace, "secrets", secretName(name, provider))
	if err := s.kubeJSON(ctx, identity, http.MethodGet, secretPath, nil, &secret); err != nil {
		return err
	}
	managedBy, _, _ := nestedString(secret, "metadata", "labels", managedByLabel)
	instance, _, _ := nestedString(secret, "metadata", "labels", instanceLabel)
	if managedBy != managedByValue || instance != name {
		return nil
	}
	return s.delete(ctx, identity, secretPath)
}

func (s *server) authorizeNamespace(user, namespace string) error {
	allowed := allowedNamespaceForUser(user, s.namespaceSuffix)
	if namespace != allowed {
		return apiError{
			StatusCode: http.StatusForbidden,
			Message:    fmt.Sprintf("user %q can only manage OpenClaw in namespace %q", user, allowed),
		}
	}
	return nil
}

func (s *server) kubeJSON(ctx context.Context, identity userIdentity, method, requestPath string, body any, out any) error {
	contentType := ""
	if method == http.MethodPatch {
		contentType = "application/apply-patch+yaml"
	} else if body != nil {
		contentType = "application/json"
	}
	return s.kubeJSONWithContentType(ctx, identity, method, requestPath, body, out, contentType)
}

func (s *server) kubeJSONWithContentType(ctx context.Context, identity userIdentity, method, requestPath string, body any, out any, contentType string) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.apiServer+requestPath, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.bearerToken)
	req.Header.Set("Accept", "application/json")
	if s.impersonate {
		req.Header.Set("Impersonate-User", identity.Name)
		for _, group := range identity.Groups {
			req.Header.Add("Impersonate-Group", group)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp.StatusCode, respBody)
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

type apiError struct {
	StatusCode int
	Message    string
}

func (e apiError) Error() string {
	return e.Message
}

func parseAPIError(statusCode int, body []byte) error {
	var status struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(body, &status); err == nil && status.Message != "" {
		return apiError{StatusCode: statusCode, Message: status.Message}
	}
	return apiError{StatusCode: statusCode, Message: http.StatusText(statusCode)}
}

func statusCodeFor(err error) int {
	var apiErr apiError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return apiErr.StatusCode
		default:
			return http.StatusBadGateway
		}
	}
	return http.StatusInternalServerError
}

func currentUser(r *http.Request) (string, error) {
	for _, header := range []string{
		"X-Forwarded-User",
		"X-Auth-Request-User",
		"X-Forwarded-Preferred-Username",
		"X-Forwarded-Email",
	} {
		if user := strings.TrimSpace(r.Header.Get(header)); user != "" {
			return user, nil
		}
	}
	if user := strings.TrimSpace(os.Getenv("DEVELOPER_USERNAME")); user != "" {
		return user, nil
	}
	return "", errors.New("OpenShift username was not forwarded to the deployer")
}

func currentIdentity(r *http.Request) (userIdentity, error) {
	user, err := currentUser(r)
	if err != nil {
		return userIdentity{}, err
	}
	return userIdentity{
		Name:   user,
		Groups: impersonationGroups(r),
	}, nil
}

func impersonationGroups(r *http.Request) []string {
	groups := []string{}
	for _, header := range []string{"X-Forwarded-Groups", "X-Auth-Request-Groups"} {
		for _, value := range r.Header.Values(header) {
			for _, group := range strings.Split(value, ",") {
				group = strings.TrimSpace(group)
				if group != "" {
					groups = appendUnique(groups, group)
				}
			}
		}
	}
	groups = appendUnique(groups, "system:authenticated")
	groups = appendUnique(groups, "system:authenticated:oauth")
	return groups
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return map[string]any{}
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		if child, ok := value.(map[string]any); ok {
			dst[key] = cloneMap(child)
			continue
		}
		dst[key] = value
	}
	return dst
}

func ensureMap(parent map[string]any, key string) map[string]any {
	child, ok := parent[key].(map[string]any)
	if !ok {
		child = map[string]any{}
		parent[key] = child
	}
	return child
}

func validateNamespace(namespace string) error {
	return validateResourceName(namespace, "namespace")
}

func validateResourceName(name, field string) error {
	if name == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(name) > 63 || !namespaceRE.MatchString(name) {
		return fmt.Errorf("%s must be a valid Kubernetes resource name", field)
	}
	return nil
}

func normalizeModelRef(provider, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		model = defaultModelForProvider(provider)
	}
	if strings.Contains(model, "/") {
		if provider == "openrouter" && !strings.HasPrefix(model, "openrouter/") {
			return "openrouter/" + model
		}
		return model
	}
	if provider == "openrouter" {
		return "openrouter/" + model
	}
	return provider + "/" + model
}

func defaultModelForProvider(provider string) string {
	switch provider {
	case "anthropic":
		return "claude-sonnet-4-6"
	case "google":
		return "gemini-3.1-pro-preview"
	case "openai":
		return "gpt-5.5"
	case "openrouter":
		return "openrouter/anthropic/claude-sonnet-4-6"
	case "xai":
		return "grok-4.3"
	default:
		return "anthropic/claude-sonnet-4-6"
	}
}

func secretName(name, provider string) string {
	return "openclaw-" + name + "-" + provider + "-api-key"
}

func allowedNamespaceForUser(username, suffix string) string {
	name := strings.ToLower(strings.TrimSpace(username))
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	name = dnsCharRE.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if suffix != "" && !strings.HasSuffix(name, suffix) {
		name += suffix
	}
	return name
}

func apiPath(parts ...string) string {
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		for _, subpart := range strings.Split(part, "/") {
			if subpart != "" {
				escaped = append(escaped, url.PathEscape(subpart))
			}
		}
	}
	return "/" + path.Join(escaped...)
}

func readyCondition(claw map[string]any) (bool, string, string) {
	conditions, _, _ := nestedSlice(claw, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["type"] != "Ready" {
			continue
		}
		reason, _ := condition["reason"].(string)
		message, _ := condition["message"].(string)
		status, _ := condition["status"].(string)
		return status == "True", reason, message
	}
	return false, "", "Waiting for status"
}

func credentialProviders(claw map[string]any) []string {
	credentials, _, _ := nestedSlice(claw, "spec", "credentials")
	providers := make([]string, 0, len(credentials))
	for _, credential := range credentials {
		credentialMap, ok := credential.(map[string]any)
		if !ok {
			continue
		}
		provider, _ := credentialMap["provider"].(string)
		if provider != "" {
			providers = append(providers, provider)
		}
	}
	return providers
}

func firstAgentName(claw map[string]any) string {
	agents, _, _ := nestedSlice(claw, "spec", "config", "raw", "agents", "list")
	if len(agents) == 0 {
		return ""
	}
	first, ok := agents[0].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := first["name"].(string)
	return name
}

func nestedString(obj map[string]any, fields ...string) (string, bool, error) {
	v, ok, err := nestedValue(obj, fields...)
	if err != nil || !ok {
		return "", ok, err
	}
	s, ok := v.(string)
	return s, ok, nil
}

func nestedSlice(obj map[string]any, fields ...string) ([]any, bool, error) {
	v, ok, err := nestedValue(obj, fields...)
	if err != nil || !ok {
		return nil, ok, err
	}
	s, ok := v.([]any)
	return s, ok, nil
}

func nestedMap(obj map[string]any, fields ...string) (map[string]any, bool, error) {
	v, ok, err := nestedValue(obj, fields...)
	if err != nil || !ok {
		return nil, ok, err
	}
	m, ok := v.(map[string]any)
	return m, ok, nil
}

func nestedValue(obj map[string]any, fields ...string) (any, bool, error) {
	var current any = obj
	for _, field := range fields {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("field %q is not an object", field)
		}
		current, ok = m[field]
		if !ok {
			return nil, false, nil
		}
	}
	return current, true, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
