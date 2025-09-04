package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gcli2api/internal/auth"
	"gcli2api/internal/codeassist"
	"gcli2api/internal/config"
	"gcli2api/internal/server"
	"gcli2api/internal/state"
	"gcli2api/internal/utils"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	// These are public values, not secrets
	oauthClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	oauthClientSecret = "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"
)

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	var cfgPath string

	rootCmd := &cobra.Command{
		Use:          "gemini-cli-2api",
		Short:        "Gemini Cli To HTTP API",
		SilenceUsage: true,
	}
	rootCmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", "config.json", "Path to config file")

	// check command: validate config and report
	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Validate configuration file",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig(cfgPath)
			if err != nil {
				return err
			}
			if err := cfg.Validate(cfgPath); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "config OK")
			return nil
		},
	}

	// server command: validate config then start server
	serverCmd := &cobra.Command{
		Use:   "server",
		Short: "Start HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load configuration from resolved path
			cfg, err := config.LoadConfig(cfgPath)
			if err != nil {
				return err
			}
			if err := cfg.Validate(cfgPath); err != nil {
				return err
			}

			// Apply optional user agent override for upstream requests
			if ua := strings.TrimSpace(cfg.UserAgent); ua != "" {
				codeassist.UserAgent = ua
			}

			// Parse optional proxy and kick off async TCP liveness check
			var proxyURL *url.URL
			if cfg.Proxy != "" {
				u, err := url.Parse(cfg.Proxy)
				if err != nil {
					return fmt.Errorf("invalid proxy URL: %w", err)
				}
				logrus.Infof("using upstream proxy: %s", cfg.Proxy)
				proxyURL = u
				go func(u *url.URL) {
					host := u.Host
					// Ensure port; if missing, default based on scheme
					_, _, err := net.SplitHostPort(host)
					if err != nil {
						if u.Scheme == "http" {
							host = net.JoinHostPort(host, "80")
						} else if u.Scheme == "socks5" {
							host = net.JoinHostPort(host, "1080")
						}
					}
					conn, err := net.DialTimeout("tcp", host, 5*time.Second)
					if err != nil {
						logrus.Warnf("proxy tcp check failed: %v", err)
						return
					}
					_ = conn.Close()
					logrus.Info("tcp check for proxy is successful")
				}(u)
			}

			// OAuth2 setup (used for all credentials)
			oauthCfg := oauth2.Config{
				ClientID:     oauthClientID,
				ClientSecret: oauthClientSecret,
				Scopes:       []string{"https://www.googleapis.com/auth/cloud-platform"},
				Endpoint:     google.Endpoint,
			}

			// Determine credential sources (multi-credential only)
			var sources []codeassist.CredSource
			if len(cfg.GeminiCredsFilePaths) == 0 {
				return fmt.Errorf("no geminiOauthCredsFiles configured; provide at least one path")
			}
			for _, p := range cfg.GeminiCredsFilePaths {
				if p == "" {
					continue
				}
				rt, xp, err := auth.LoadRawTokenFromFile(p)
				if err != nil {
					logrus.Errorf("failed to load credential %q: %v", p, err)
					continue
				}
				sources = append(sources, codeassist.CredSource{Path: xp, Raw: rt, Persist: true})
			}
			if len(sources) == 0 {
				return fmt.Errorf("no usable credentials from geminiOauthCredsFiles")
			}

			// Ensure SQLitePath parent directory exists
			if dir := filepath.Dir(cfg.SQLitePath); dir != "." && dir != "" {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return fmt.Errorf("failed to create SQLite directory %q: %w", dir, err)
				}
			}

			// Initialize SQLite state store
			st, err := state.Open(cfg.SQLitePath)
			if err != nil {
				logrus.Warnf("SQLite open error (using memory-only cache): %v", err)
			}

			// Normalize projectIds map keys via ~ expansion only (no symlink resolution)
			normalizedProjectMap := make(map[string][]string)
			for k, v := range cfg.ProjectIds {
				xp, err := utils.ExpandUser(k)
				if err != nil {
					// Should have been validated; continue with raw key on error
					xp = k
				}
				normalizedProjectMap[xp] = v
			}

			// Build MultiClient (works for both single and multi-cred cases)
			mc, err := codeassist.NewMultiClient(oauthCfg, sources, cfg.RequestMaxRetries, time.Duration(cfg.RequestBaseDelayMillis)*time.Millisecond, st, proxyURL, normalizedProjectMap)
			if err != nil {
				return fmt.Errorf("failed to init client: %w", err)
			}

			// Build server using injected CodeAssist client
			srv := server.NewWithCAClient(cfg, mc)

			addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.ServerPort)
			httpSrv := &http.Server{
				Addr:              addr,
				Handler:           srv.Router(),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       10 * time.Minute,
				WriteTimeout:      10 * time.Minute,
				IdleTimeout:       120 * time.Second,
				ErrorLog:          log.New(logrus.StandardLogger().WriterLevel(logrus.ErrorLevel), "http: ", 0),
			}

			logrus.Infof("gcli2api listening on http://%s", addr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("server error: %w", err)
			}
			return nil
		},
	}

	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(checkCmd)

	if err := rootCmd.Execute(); err != nil {
		logrus.Fatalf("%v", err)
	}
}

// no extra helpers needed; cobra commands wire directly to server startup.
