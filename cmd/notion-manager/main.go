package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"notion-manager/internal/proxy"
)

func requiresAPIKey(path string) bool {
	return path == "/models" || strings.HasPrefix(path, "/v1/")
}

func apiKeyAuthMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresAPIKey(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		var key string
		if bearer := r.Header.Get("Authorization"); bearer != "" {
			key = strings.TrimPrefix(bearer, "Bearer ")
			if key == bearer {
				key = ""
			}
		}
		if key == "" {
			key = r.Header.Get("x-api-key")
		}
		if key == "" {
			http.Error(w, `{"error":{"message":"missing api key, use 'Authorization: Bearer <key>' or 'x-api-key: <key>'","type":"auth_error"}}`, http.StatusUnauthorized)
			return
		}
		if key != apiKey {
			http.Error(w, `{"error":{"message":"invalid api key","type":"auth_error"}}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newMux(pool *proxy.AccountPool, accountsDir string, apiKey string, dashAuth *proxy.DashboardAuth) *http.ServeMux {
	mux := http.NewServeMux()

	// Anthropic + OpenAI-compatible API endpoints
	mux.HandleFunc("/v1/messages", proxy.HandleAnthropicMessages(pool))
	mux.HandleFunc("/v1/chat/completions", proxy.HandleOpenAIChatCompletions(pool))
	mux.HandleFunc("/v1/responses", proxy.HandleOpenAIResponses(pool))
	mux.HandleFunc("/v1/models", proxy.HandlePublicModels(pool))
	mux.HandleFunc("/models", proxy.HandlePublicModels(pool))

	// Health check with quota details
	mux.HandleFunc("/health", proxy.HandleHealth(pool))

	// Admin API endpoints
	mux.HandleFunc("/admin/accounts", proxy.HandleAdminAccounts(pool, dashAuth))
	mux.HandleFunc("/admin/accounts/add", proxy.HandleAddAccount(pool, accountsDir, dashAuth))
	mux.HandleFunc("/admin/accounts/delete", proxy.HandleDeleteAccount(pool, accountsDir, dashAuth))
	mux.HandleFunc("/admin/models", proxy.HandleAdminModels(pool, dashAuth))
	mux.HandleFunc("/admin/refresh", proxy.HandleAdminRefresh(pool, accountsDir, dashAuth))
	mux.HandleFunc("/admin/settings", proxy.HandleAdminSettings("config.yaml", dashAuth))

	// Dashboard (React SPA with embedded API key + auth)
	mux.Handle("/dashboard/", proxy.HandleDashboard(apiKey, dashAuth))
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
	})

	// Proxy start: create targeted session for a specific account (requires dashboard auth)
	rp := proxy.NewReverseProxy(pool)
	mux.HandleFunc("/proxy/start", proxy.HandleProxyStart(pool, rp, dashAuth))

	// Catch-all: reverse proxy for paths with valid np_session, 404 for everything else
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp.ServeHTTP(w, r)
	}))

	return mux
}

func main() {
	// Load configuration: config.yaml > env > defaults
	cfg, err := proxy.LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("[config] %v", err)
	}

	// Ensure API key exists (generate + write back if missing)
	proxy.EnsureApiKey(cfg, "config.yaml")

	// Auto-generate admin_password if not set
	if cfg.Server.AdminPassword == "" {
		generated := proxy.GenerateAdminPassword()
		cfg.Server.AdminPassword = generated
		log.Printf("[config] no admin_password configured, generated: %s", generated)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  ========================================")
		fmt.Fprintf(os.Stderr, "  Dashboard password: %s\n", generated)
		fmt.Fprintln(os.Stderr, "  Please save this password. It will be")
		fmt.Fprintln(os.Stderr, "  hashed and cannot be recovered later.")
		fmt.Fprintln(os.Stderr, "  ========================================")
		fmt.Fprintln(os.Stderr, "")
	}

	// Hash admin password on first run (plaintext → SHA256+salt)
	proxy.EnsureAdminPassword(cfg, "config.yaml")

	// Apply config to package-level variables
	proxy.ApplyConfig(cfg)

	port := cfg.Server.Port
	accountsDir := cfg.Server.AccountsDir
	tokenFile := cfg.Server.TokenFile

	// Load account pool
	pool := proxy.NewAccountPool()

	if _, err := os.Stat(accountsDir); err == nil {
		if err := pool.LoadFromDir(accountsDir); err != nil {
			log.Printf("[warn] %v", err)
		}
	}

	// Fallback: load from token file or env
	if pool.Count() == 0 {
		tokenV2 := os.Getenv("NOTION_TOKEN_V2")
		if tokenV2 == "" {
			if data, err := os.ReadFile(tokenFile); err == nil {
				tokenV2 = strings.TrimSpace(string(data))
			}
		}
		if tokenV2 != "" {
			pool.LoadSingle(tokenFile)
		}
	}

	if pool.Count() == 0 {
		log.Printf("[warn] No accounts found. Place account JSON files in %s/ to enable API and proxy.", accountsDir)
	}

	// Startup refresh: check quota + fetch models for all accounts in the
	// background. The HTTP listener must come up immediately — large
	// account pools can take minutes to refresh, and we have a per-request
	// live quota check that disables exhausted accounts before they can
	// serve traffic, plus the refresh worker itself marks accounts as
	// exhausted via applyQuotaInfo as soon as a check completes.
	if pool.Count() > 0 {
		log.Printf("[startup] kicking off background quota refresh for %d account(s)", pool.Count())
		go pool.RefreshAll(accountsDir)
	}

	// Background refresh loop
	pool.StartRefreshLoop(cfg.RefreshInterval(), accountsDir)

	// CORS middleware
	cors := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, X-Web-Search, X-Workspace-Search")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	apiKey := cfg.Server.ApiKey
	// Dashboard auth (admin password from config.yaml)
	dashAuth := proxy.NewDashboardAuth(cfg.Server.AdminPassword, apiKey)
	mux := newMux(pool, accountsDir, apiKey, dashAuth)

	log.Printf("=== notion-manager ===")
	log.Printf("Listening on :%s", port)
	log.Printf("Accounts: %d", pool.Count())
	log.Printf("API Key: %s", apiKey)
	log.Printf("Dashboard: password protected")
	log.Printf("Endpoints:")
	log.Printf("  GET  /dashboard/    (Dashboard UI)")
	log.Printf("  GET  /proxy/start   (Open proxy for account)")
	log.Printf("  POST /v1/messages           (Anthropic Messages API)")
	log.Printf("  POST /v1/chat/completions   (OpenAI Chat Completions API)")
	log.Printf("  POST /v1/responses          (OpenAI Responses API)")
	log.Printf("  GET  /v1/models             (OpenAI models API)")
	log.Printf("  GET  /models                (OpenAI models alias)")
	log.Printf("  GET  /health")
	log.Printf("  GET  /admin/accounts")
	log.Printf("  GET  /admin/models")
	log.Printf("  GET  /admin/settings  (search settings)")
	log.Printf("  GET  /ai            (Reverse Proxy → notion.so)")

	if err := http.ListenAndServe(":"+port, cors(apiKeyAuthMiddleware(apiKey, mux))); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
