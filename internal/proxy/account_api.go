package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DiscoverAccountFromToken calls Notion APIs using the given token_v2 to discover
// all account information (user, space, models, quota).
func DiscoverAccountFromToken(tokenV2 string) (*Account, error) {
	client := getChromeHTTPClient(AppConfig.APITimeoutDuration())

	// Step 1: Call loadUserContent to get user/space info
	req, err := http.NewRequest("POST", NotionAPIBase+"/loadUserContent", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("create loadUserContent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", "token_v2="+tokenV2)
	req.Header.Set("User-Agent", AppConfig.Browser.UserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loadUserContent request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("loadUserContent API error %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	// Parse the response
	var userData struct {
		RecordMap struct {
			NotionUser  map[string]json.RawMessage `json:"notion_user"`
			UserRoot    map[string]json.RawMessage `json:"user_root"`
			Space       map[string]json.RawMessage `json:"space"`
			UserSetting map[string]json.RawMessage `json:"user_settings"`
		} `json:"recordMap"`
	}
	if err := json.Unmarshal(body, &userData); err != nil {
		return nil, fmt.Errorf("parse loadUserContent: %w", err)
	}

	// Extract user ID and info
	var userID, userName, userEmail string
	for id, raw := range userData.RecordMap.NotionUser {
		userID = id
		var u struct {
			Value struct {
				Value *struct {
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"value"`
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &u); err == nil {
			if u.Value.Value != nil {
				userName = u.Value.Value.Name
				userEmail = u.Value.Value.Email
			} else {
				userName = u.Value.Name
				userEmail = u.Value.Email
			}
		}
		break
	}
	if userID == "" {
		return nil, fmt.Errorf("no user found in loadUserContent response")
	}

	// Extract space view pointers from user_root
	type spaceViewPointer struct {
		SpaceID string `json:"spaceId"`
		ID      string `json:"id"`
	}
	var spaceViewPointers []spaceViewPointer
	if raw, ok := userData.RecordMap.UserRoot[userID]; ok {
		var ur struct {
			Value struct {
				Value *struct {
					SpaceViewPointers []spaceViewPointer `json:"space_view_pointers"`
				} `json:"value"`
				SpaceViewPointers []spaceViewPointer `json:"space_view_pointers"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &ur); err == nil {
			if ur.Value.Value != nil {
				spaceViewPointers = ur.Value.Value.SpaceViewPointers
			} else {
				spaceViewPointers = ur.Value.SpaceViewPointers
			}
		}
	}

	// Find the best space (AI enabled, non-free preferred)
	type spaceInfo struct {
		ID          string
		Name        string
		PlanType    string
		SpaceViewID string
		AIEnabled   bool
	}
	var bestSpace *spaceInfo
	for _, ptr := range spaceViewPointers {
		raw, ok := userData.RecordMap.Space[ptr.SpaceID]
		if !ok {
			continue
		}
		var s struct {
			Value struct {
				Value *struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					PlanType string `json:"plan_type"`
					Settings struct {
						EnableAIFeature  *bool `json:"enable_ai_feature"`
						DisableAIFeature *bool `json:"disable_ai_feature"`
					} `json:"settings"`
				} `json:"value"`
				ID       string `json:"id"`
				Name     string `json:"name"`
				PlanType string `json:"plan_type"`
				Settings struct {
					EnableAIFeature  *bool `json:"enable_ai_feature"`
					DisableAIFeature *bool `json:"disable_ai_feature"`
				} `json:"settings"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		var si spaceInfo
		si.SpaceViewID = ptr.ID
		if s.Value.Value != nil {
			si.ID = s.Value.Value.ID
			si.Name = s.Value.Value.Name
			si.PlanType = s.Value.Value.PlanType
			aiOff := s.Value.Value.Settings.DisableAIFeature != nil && *s.Value.Value.Settings.DisableAIFeature
			si.AIEnabled = !aiOff
		} else {
			si.ID = s.Value.ID
			si.Name = s.Value.Name
			si.PlanType = s.Value.PlanType
			aiOff := s.Value.Settings.DisableAIFeature != nil && *s.Value.Settings.DisableAIFeature
			si.AIEnabled = !aiOff
		}
		if si.ID == "" {
			si.ID = ptr.SpaceID
		}
		if bestSpace == nil || (si.AIEnabled && si.PlanType != "free") {
			bestSpace = &si
		}
	}
	if bestSpace == nil {
		return nil, fmt.Errorf("no workspace found for this account")
	}

	// Extract timezone from user_settings
	timezone := "UTC"
	if raw, ok := userData.RecordMap.UserSetting[userID]; ok {
		var us struct {
			Value struct {
				Value *struct {
					Settings struct {
						TimeZone string `json:"time_zone"`
					} `json:"settings"`
				} `json:"value"`
				Settings struct {
					TimeZone string `json:"time_zone"`
				} `json:"settings"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &us); err == nil {
			if us.Value.Value != nil && us.Value.Value.Settings.TimeZone != "" {
				timezone = us.Value.Value.Settings.TimeZone
			} else if us.Value.Settings.TimeZone != "" {
				timezone = us.Value.Settings.TimeZone
			}
		}
	}

	browserID := generateUUIDv4()
	deviceID := generateUUIDv4()

	acc := &Account{
		TokenV2:       tokenV2,
		UserID:        userID,
		UserName:      userName,
		UserEmail:     userEmail,
		SpaceID:       bestSpace.ID,
		SpaceName:     bestSpace.Name,
		SpaceViewID:   bestSpace.SpaceViewID,
		PlanType:      bestSpace.PlanType,
		Timezone:      timezone,
		ClientVersion: DefaultClientVersion,
		BrowserID:     browserID,
		DeviceID:      deviceID,
	}

	// Step 2: Fetch available models
	models, err := FetchModels(acc)
	if err != nil {
		log.Printf("[add-account] model fetch failed (non-fatal): %v", err)
	} else {
		acc.setModels(models)
	}

	// Step 3: Check quota
	quota, err := CheckQuota(acc)
	if err != nil {
		log.Printf("[add-account] quota check failed (non-fatal): %v", err)
	} else {
		now := time.Now()
		acc.setQuotaInfo(quota, &now)
	}

	return acc, nil
}

// SaveAccountToFile writes an Account to a JSON file in the accounts directory.
func SaveAccountToFile(acc *Account, dir string) (string, error) {
	// Generate a safe filename from the email or username
	name := acc.UserEmail
	if name == "" {
		name = acc.UserName
	}
	if name == "" {
		name = acc.UserID
	}
	// Sanitize filename
	name = strings.Map(func(r rune) rune {
		if r == '@' || r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, name)

	filename := name + ".json"
	path := filepath.Join(dir, filename)

	// Build the JSON structure
	data := map[string]interface{}{
		"token_v2":       acc.TokenV2,
		"user_id":        acc.UserID,
		"user_name":      acc.UserName,
		"user_email":     acc.UserEmail,
		"space_id":       acc.SpaceID,
		"space_name":     acc.SpaceName,
		"space_view_id":  acc.SpaceViewID,
		"plan_type":      acc.PlanType,
		"timezone":       acc.Timezone,
		"client_version": acc.ClientVersion,
		"browser_id":     acc.BrowserID,
		"device_id":      acc.DeviceID,
	}
	modelSnapshot := acc.modelsSnapshot()
	quota := acc.quotaSnapshot()
	if len(modelSnapshot) > 0 {
		var models []map[string]string
		for _, m := range modelSnapshot {
			models = append(models, map[string]string{"id": m.ID, "name": m.Name})
		}
		data["available_models"] = models
	}
	if quota.Info != nil {
		data["quota_info"] = map[string]interface{}{
			"is_eligible":         quota.Info.IsEligible,
			"space_usage":         quota.Info.SpaceUsage,
			"space_limit":         quota.Info.SpaceLimit,
			"user_usage":          quota.Info.UserUsage,
			"user_limit":          quota.Info.UserLimit,
			"last_usage_at":       quota.Info.LastUsageAtMs,
			"research_mode_usage": quota.Info.ResearchModeUsage,
			"has_premium":         quota.Info.HasPremium,
			"premium_balance":     quota.Info.PremiumBalance,
			"premium_usage":       quota.Info.PremiumUsage,
			"premium_limit":       quota.Info.PremiumLimit,
		}
	}
	if quota.CheckedAt != nil {
		data["quota_checked_at"] = quota.CheckedAt.Format(time.RFC3339)
	}
	data["extracted_at"] = time.Now().Format(time.RFC3339)

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal account JSON: %w", err)
	}

	// Ensure accounts directory exists
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create accounts dir: %w", err)
	}

	if err := os.WriteFile(path, append(out, '\n'), 0644); err != nil {
		return "", fmt.Errorf("write account file: %w", err)
	}

	return filename, nil
}

// AddAccount adds an account to the pool (hot-load, no restart needed).
func (p *AccountPool) AddAccount(acc *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Check for duplicate by email
	for i, existing := range p.accounts {
		if existing.UserEmail == acc.UserEmail {
			// Replace existing
			p.accounts[i] = acc
			log.Printf("[account] replaced: %s (%s)", acc.UserName, acc.UserEmail)
			return
		}
	}
	p.accounts = append(p.accounts, acc)
	log.Printf("[account] added: %s (%s) [%s]", acc.UserName, acc.UserEmail, acc.PlanType)
}

// DeleteAccountFile removes the JSON file for an account from the accounts directory.
func DeleteAccountFile(email, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read accounts dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var existing map[string]interface{}
		if err := json.Unmarshal(data, &existing); err != nil {
			continue
		}
		if e, _ := existing["user_email"].(string); e == email {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("delete file %s: %w", entry.Name(), err)
			}
			log.Printf("[account] deleted file: %s", entry.Name())
			return nil
		}
	}
	return fmt.Errorf("account file not found for %s", email)
}

// HandleAddAccount accepts a token_v2, discovers account info via Notion APIs,
// saves it to disk, and hot-loads it into the pool.
func HandleAddAccount(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// Require dashboard session
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		var body struct {
			TokenV2 string `json:"token_v2"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		tokenV2 := strings.TrimSpace(body.TokenV2)
		if tokenV2 == "" {
			http.Error(w, `{"error":"token_v2 is required"}`, http.StatusBadRequest)
			return
		}

		log.Printf("[add-account] discovering account from token_v2 (%d chars)...", len(tokenV2))

		// Discover account info
		acc, err := DiscoverAccountFromToken(tokenV2)
		if err != nil {
			log.Printf("[add-account] discovery failed: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("Failed to discover account: %v", err),
			})
			return
		}

		// Save to file
		filename, err := SaveAccountToFile(acc, accountsDir)
		if err != nil {
			log.Printf("[add-account] save failed: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("Failed to save account: %v", err),
			})
			return
		}

		// Hot-load into pool
		pool.AddAccount(acc)

		log.Printf("[add-account] success: %s (%s) → %s", acc.UserName, acc.UserEmail, filename)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "ok",
			"filename": filename,
			"account": map[string]string{
				"name":      acc.UserName,
				"email":     acc.UserEmail,
				"space":     acc.SpaceName,
				"plan_type": acc.PlanType,
			},
		})
	}
}

// HandleDeleteAccount removes an account from the pool and deletes its file.
func HandleDeleteAccount(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// Require dashboard session
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		email := strings.TrimSpace(body.Email)
		if email == "" {
			http.Error(w, `{"error":"email is required"}`, http.StatusBadRequest)
			return
		}

		// Remove from pool
		if !pool.RemoveAccountByEmail(email) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "account not found in pool"})
			return
		}

		// Delete file
		if err := DeleteAccountFile(email, accountsDir); err != nil {
			log.Printf("[delete-account] file deletion warning: %v", err)
			// Account removed from pool but file not deleted — not fatal
		}

		log.Printf("[delete-account] removed: %s", email)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
