package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

//go:embed templates/*
var templateFS embed.FS

type config struct {
	Port           string
	GitHubOrg      string
	GitHubToken    string
	RequestLimit   int
	CacheTTL       time.Duration
	RefreshTimeout time.Duration
}

type server struct {
	cfg        config
	github     *githubClient
	cache      *dashboardCache
	templates  *template.Template
	httpServer *http.Server
}

type pageData struct {
	Org              string
	OrgDisplay       string
	GeneratedAt      string
	RepoCount        int
	PullRequestCount int
	Repositories     []repositoryView
	Warnings         []string
	Error            string
}

func main() {
	cfg := loadConfig()

	templates := template.Must(template.ParseFS(templateFS, "templates/*.html"))
	srv := &server{
		cfg:       cfg,
		github:    newGitHubClient(cfg.GitHubToken, cfg.RequestLimit),
		templates: templates,
	}
	srv.cache = newDashboardCache(cfg.GitHubOrg, srv.github, cfg.CacheTTL, cfg.RefreshTimeout)

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleDashboard)
	mux.HandleFunc("/api/dashboard", srv.handleDashboardAPI)
	mux.HandleFunc("/healthz", srv.handleHealth)

	srv.httpServer = &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv.cache.start(ctx)

	go func() {
		log.Printf("review dashboard listening on http://localhost:%s for org %q", cfg.Port, cfg.GitHubOrg)
		if err := srv.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server failed: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}

func loadConfig() config {
	port := envOrDefault("PORT", "8080")
	org := strings.TrimSpace(os.Getenv("GITHUB_ORG"))
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	requestLimit := 8
	cacheTTL := 2 * time.Minute
	refreshTimeout := 90 * time.Second

	if raw := strings.TrimSpace(os.Getenv("GITHUB_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			requestLimit = parsed
		}
	}
	if raw := strings.TrimSpace(os.Getenv("GITHUB_CACHE_TTL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			cacheTTL = parsed
		}
	}
	if raw := strings.TrimSpace(os.Getenv("GITHUB_REFRESH_TIMEOUT")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			refreshTimeout = parsed
		}
	}

	if org == "" {
		log.Fatal("GITHUB_ORG is required")
	}
	if token == "" {
		log.Fatal("GITHUB_TOKEN is required")
	}

	return config{
		Port:           port,
		GitHubOrg:      org,
		GitHubToken:    token,
		RequestLimit:   requestLimit,
		CacheTTL:       cacheTTL,
		RefreshTimeout: refreshTimeout,
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	snapshot := s.cache.get(ctx)
	data := pageData{
		Org:          s.cfg.GitHubOrg,
		OrgDisplay:   displayOrgName(s.cfg.GitHubOrg),
		GeneratedAt:  snapshot.GeneratedAt.Format(time.RFC1123),
		Repositories: snapshot.Repositories,
		RepoCount:    len(snapshot.Repositories),
		Warnings:     snapshot.Warnings,
	}
	for _, repo := range snapshot.Repositories {
		data.PullRequestCount += len(repo.PullRequests)
	}
	if snapshot.Error != "" {
		data.Error = snapshot.Error
	}

	var body bytes.Buffer
	if execErr := s.templates.ExecuteTemplate(&body, "index.html", data); execErr != nil {
		http.Error(w, execErr.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if snapshot.Error != "" {
		w.WriteHeader(http.StatusBadGateway)
	}
	_, _ = w.Write(body.Bytes())
}

func (s *server) handleDashboardAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	snapshot := s.cache.get(ctx)
	resp := struct {
		Org          string           `json:"org"`
		GeneratedAt  time.Time        `json:"generatedAt"`
		Repositories []repositoryView `json:"repositories"`
		Warnings     []string         `json:"warnings,omitempty"`
		Error        string           `json:"error,omitempty"`
	}{
		Org:          s.cfg.GitHubOrg,
		GeneratedAt:  snapshot.GeneratedAt,
		Repositories: snapshot.Repositories,
		Warnings:     snapshot.Warnings,
	}

	if snapshot.Error != "" {
		resp.Error = snapshot.Error
	}

	body, encodeErr := json.Marshal(resp)
	if encodeErr != nil {
		http.Error(w, encodeErr.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if snapshot.Error != "" {
		w.WriteHeader(http.StatusBadGateway)
	}
	_, _ = w.Write(body)
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func sortRepositories(repositories []repositoryView) {
	sort.Slice(repositories, func(i, j int) bool {
		if len(repositories[i].PullRequests) == len(repositories[j].PullRequests) {
			return repositories[i].Name < repositories[j].Name
		}
		return len(repositories[i].PullRequests) > len(repositories[j].PullRequests)
	})
}

func displayOrgName(org string) string {
	org = strings.TrimSpace(org)
	if org == "" {
		return ""
	}

	firstRune, size := utf8.DecodeRuneInString(org)
	if firstRune == utf8.RuneError && size == 0 {
		return ""
	}

	return string(unicode.ToUpper(firstRune)) + org[size:]
}
