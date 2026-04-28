package msalogin

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// proofsDumpDir is where we drop HTML snapshots when the proofs flow
// fails to find a form selector it expects. Operators can hand the
// resulting file to a developer to update the regex / flow.
//
// Path is relative to the process's working directory, which for
// notion-manager is the project root. We deliberately do not put it
// under accounts/ to avoid accidentally getting committed alongside
// account JSONs.
const proofsDumpDir = "logs/proofs_debug"

// dumpProofsDebug writes the failing HTML + URL to disk and returns
// the path (or "" on best-effort failure). We never crash the proofs
// flow over a dump failure; the caller still gets a useful error.
func dumpProofsDebug(c *Client, reason, currentURL, html string) string {
	if html == "" {
		return ""
	}
	if err := os.MkdirAll(proofsDumpDir, 0o755); err != nil {
		log.Printf("[proofs-debug] mkdir %s: %v", proofsDumpDir, err)
		return ""
	}

	stamp := time.Now().UTC().Format("20060102T150405Z")
	email := "unknown"
	if c != nil && c.main.Email != "" {
		email = sanitizeForFilename(c.main.Email)
	}
	name := fmt.Sprintf("%s_%s_%s.html", stamp, email, reason)
	full := filepath.Join(proofsDumpDir, name)

	header := fmt.Sprintf("<!--\n  account:    %s\n  current_url: %s\n  reason:     %s\n  dumped_at:  %s\n-->\n",
		email, currentURL, reason, stamp)
	if err := os.WriteFile(full, []byte(header+html), 0o644); err != nil {
		log.Printf("[proofs-debug] write %s: %v", full, err)
		return ""
	}
	log.Printf("[proofs-debug] wrote %s (%d bytes, url=%s)", full, len(html), truncate(currentURL, 100))
	return full
}

// sanitizeForFilename strips characters that Windows / Linux refuse in
// filenames (notably @, : and /).
func sanitizeForFilename(s string) string {
	repl := strings.NewReplacer(
		"@", "_",
		":", "_",
		"/", "_",
		"\\", "_",
		"?", "_",
		"*", "_",
		`"`, "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	return repl.Replace(s)
}
