// Command register provisions Notion accounts via Microsoft SSO using
// "<email>----<password>----<client_id>----<refresh_token>" credentials,
// then writes one notion-manager-compatible JSON file per success into the
// configured accounts directory.
//
// Usage:
//
//	notion-manager-register [-accounts ./accounts] [-input creds.txt]
//
// When -input is omitted, lines are read from stdin (so the CLI can be
// piped or fed via heredoc).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"notion-manager/internal/msalogin"
)

func main() {
	accountsDir := flag.String("accounts", "accounts", "directory to drop generated account JSON files into")
	inputFile := flag.String("input", "", "file with bulk credentials (one '----'-separated line per account); empty = stdin")
	timeout := flag.Duration("timeout", 5*time.Minute, "maximum total time per account (login + onboarding)")
	dryRun := flag.Bool("dry-run", false, "parse + log credentials but do not actually log in")
	proxyURL := flag.String("proxy", "", "outbound proxy URL (http/https/socks5); empty = direct")
	dumpCookies := flag.String("dump-cookies", "", "stop after token_v2 (no onboarding) and write notion.so cookies as JSON to this path")
	flag.Parse()

	raw, err := readInput(*inputFile)
	if err != nil {
		log.Fatalf("[register] read input: %v", err)
	}

	tokens, err := msalogin.ParseTokens(raw)
	if err != nil {
		log.Fatalf("[register] parse: %v", err)
	}
	if len(tokens) == 0 {
		log.Fatalf("[register] no credentials parsed")
	}

	if err := os.MkdirAll(*accountsDir, 0o755); err != nil {
		log.Fatalf("[register] mkdir %s: %v", *accountsDir, err)
	}

	backups := msalogin.PairBackups(tokens)
	results := struct {
		ok, failed int
	}{}

	for i, tok := range tokens {
		log.Printf("[register %d/%d] %s", i+1, len(tokens), tok)
		if *dryRun {
			continue
		}
		c, err := msalogin.New(tok, msalogin.Options{
			Backup:   backups[i],
			Timeout:  *timeout / 10,
			ProxyURL: *proxyURL,
		})
		if err != nil {
			log.Printf("[register %d/%d] init: %v", i+1, len(tokens), err)
			results.failed++
			continue
		}
		if *dumpCookies != "" {
			if err := c.LoginUntilTokenV2(); err != nil {
				log.Printf("[register %d/%d] FAIL %s: %v", i+1, len(tokens), tok.Email, err)
				results.failed++
				continue
			}
			cookies := c.ExportNotionCookies()
			path := *dumpCookies
			if len(tokens) > 1 {
				path = fmt.Sprintf("%s.%d", *dumpCookies, i+1)
			}
			data, _ := json.MarshalIndent(cookies, "", "  ")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				log.Printf("[register %d/%d] dump-cookies write %s: %v", i+1, len(tokens), path, err)
				results.failed++
				continue
			}
			log.Printf("[register %d/%d] OK %s → %s (%d cookies, no onboarding)",
				i+1, len(tokens), tok.Email, path, len(cookies))
			results.ok++
			continue
		}
		session, err := c.Login()
		if err != nil {
			log.Printf("[register %d/%d] FAIL %s: %v", i+1, len(tokens), tok.Email, err)
			results.failed++
			continue
		}
		path := filepath.Join(*accountsDir, accountFilename(tok.Email))
		if err := writeAccount(path, session); err != nil {
			log.Printf("[register %d/%d] write %s: %v", i+1, len(tokens), path, err)
			results.failed++
			continue
		}
		log.Printf("[register %d/%d] OK %s → %s (space=%s plan=%s)",
			i+1, len(tokens), tok.Email, path, session.SpaceID, session.PlanType)
		results.ok++
	}

	log.Printf("[register] done: %d ok, %d failed (out of %d)", results.ok, results.failed, len(tokens))
	if results.failed > 0 {
		os.Exit(1)
	}
}

// readInput returns the contents of path, or stdin when path is empty.
func readInput(path string) (string, error) {
	if path != "" {
		data, err := os.ReadFile(path)
		return string(data), err
	}
	stat, err := os.Stdin.Stat()
	if err == nil && (stat.Mode()&os.ModeCharDevice) != 0 {
		fmt.Fprintln(os.Stderr,
			"Paste credentials (one per line, '----'-separated), then press Enter and Ctrl+Z + Enter to finish:")
	}
	return readAll(bufio.NewReader(os.Stdin))
}

func readAll(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	return string(data), err
}

var safeFileChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// accountFilename derives a filesystem-safe JSON filename from an email.
func accountFilename(email string) string {
	clean := safeFileChars.ReplaceAllString(strings.ToLower(email), "_")
	clean = strings.Trim(clean, "_")
	if clean == "" {
		clean = "account"
	}
	return clean + ".json"
}

// writeAccount serializes the session as the schema notion-manager expects.
func writeAccount(path string, s *msalogin.NotionSession) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
