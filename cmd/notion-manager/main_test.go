package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"notion-manager/internal/proxy"
)

func TestRequiresAPIKey(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/v1/messages", want: true},
		{path: "/v1/chat/completions", want: true},
		{path: "/v1/responses", want: true},
		{path: "/v1/models", want: true},
		{path: "/models", want: true},
		{path: "/health", want: false},
		{path: "/dashboard/", want: false},
	}

	for _, tc := range tests {
		if got := requiresAPIKey(tc.path); got != tc.want {
			t.Fatalf("requiresAPIKey(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestAPIKeyAuthMiddleware_ProtectsModelsRoutes(t *testing.T) {
	handler := apiKeyAuthMiddleware("sk-test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{name: "models missing key", path: "/models", want: http.StatusUnauthorized},
		{name: "models wrong key", path: "/models", headers: map[string]string{"Authorization": "Bearer sk-wrong"}, want: http.StatusUnauthorized},
		{name: "models bearer", path: "/models", headers: map[string]string{"Authorization": "Bearer sk-test"}, want: http.StatusNoContent},
		{name: "v1 models x-api-key", path: "/v1/models", headers: map[string]string{"x-api-key": "sk-test"}, want: http.StatusNoContent},
		{name: "chat missing key", path: "/v1/chat/completions", want: http.StatusUnauthorized},
		{name: "responses x-api-key", path: "/v1/responses", headers: map[string]string{"x-api-key": "sk-test"}, want: http.StatusNoContent},
		{name: "health no auth", path: "/health", want: http.StatusNoContent},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		for key, value := range tc.headers {
			req.Header.Set(key, value)
		}

		handler.ServeHTTP(rec, req)

		if rec.Code != tc.want {
			t.Fatalf("%s: expected %d, got %d body=%s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
}

func TestNewMux_RegistersModelsRoutes(t *testing.T) {
	original := proxy.SnapshotModelMap()
	proxy.ReplaceModelMap(map[string]string{
		"opus-4.6": "avocado-froyo-medium",
	})
	t.Cleanup(func() {
		proxy.ReplaceModelMap(original)
	})

	pool := proxy.NewAccountPool()
	dashAuth := proxy.NewDashboardAuth("", "sk-test")
	usageStats := proxy.InitUsageStats("")
	regDeps := &proxy.RegisterJobsDeps{Pool: pool, AccountsDir: "", Auth: dashAuth}
	mux := newMux(pool, "", "sk-test", dashAuth, usageStats, regDeps)
	handler := apiKeyAuthMiddleware("sk-test", mux)

	for _, path := range []string{"/v1/models", "/models"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer sk-test")
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestNewMux_RegistersOpenAIRoutes(t *testing.T) {
	originalConfig := proxy.AppConfig
	proxy.AppConfig = proxy.DefaultConfig()
	t.Cleanup(func() {
		proxy.AppConfig = originalConfig
	})

	pool := proxy.NewAccountPool()
	dashAuth := proxy.NewDashboardAuth("", "sk-test")
	usageStats := proxy.InitUsageStats("")
	regDeps := &proxy.RegisterJobsDeps{Pool: pool, AccountsDir: "", Auth: dashAuth}
	mux := newMux(pool, "", "sk-test", dashAuth, usageStats, regDeps)
	handler := apiKeyAuthMiddleware("sk-test", mux)

	tests := []struct {
		path string
		body string
	}{
		{path: "/v1/chat/completions", body: `{"messages":[]}`},
		{path: "/v1/responses", body: `{"input":"ping"}`},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(rec, req)

		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s: expected registered handler, got 404", tc.path)
		}
	}
}
