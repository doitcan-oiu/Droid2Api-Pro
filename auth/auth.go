package auth

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"droid2api/config"
)

const (
	refreshURL = "https://api.workos.com/user_management/authenticate"
	clientID   = "client_01HNM792M5G5G1A2THWPXKFMXB"
)

// tokenSlot represents one refresh-key slot with its own access token.
type tokenSlot struct {
	mu             sync.RWMutex
	refreshToken   string
	accessToken    string
	lastRefresh    time.Time
	index          int
	disabled       bool   // true = 401/403, permanently disabled
	disabledReason string // reason for disabling
}

// Manager handles multi-token round-robin with session binding.
type Manager struct {
	mu       sync.RWMutex
	slots    []*tokenSlot
	rrIndex  uint64 // atomic round-robin counter
	sessions sync.Map // sessionID -> slotIndex (for session binding)

	httpClient *http.Client
}

var globalManager *Manager

// Initialize creates the global token manager and performs initial refresh on all keys.
func Initialize() error {
	c := config.Get()
	if len(c.RefreshKeys) == 0 {
		log.Println("[WARN] No refresh keys configured. Client authorization mode only.")
		globalManager = &Manager{
			httpClient: &http.Client{Timeout: 30 * time.Second},
		}
		return nil
	}

	slots := make([]*tokenSlot, len(c.RefreshKeys))
	for i, key := range c.RefreshKeys {
		slots[i] = &tokenSlot{
			refreshToken: key,
			index:        i,
		}
		// Try to load saved token — the saved refresh_token is newer than config.yaml
		loadSavedSlot(slots[i])
	}

	mgr := &Manager{
		slots:      slots,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}

	// Refresh all tokens on startup (concurrently)
	var wg sync.WaitGroup
	errCh := make(chan error, len(slots))
	for _, slot := range slots {
		wg.Add(1)
		go func(s *tokenSlot) {
			defer wg.Done()
			if err := mgr.refreshSlot(s); err != nil {
				errCh <- fmt.Errorf("slot[%d]: %w", s.index, err)
			}
		}(slot)
	}
	wg.Wait()
	close(errCh)

	// Collect errors but don't fail if at least one token works
	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
	}
	if len(errs) == len(slots) {
		return fmt.Errorf("all tokens failed to refresh: %s", strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		log.Printf("[WARN] Some tokens failed to refresh: %s", strings.Join(errs, "; "))
	}

	globalManager = mgr
	log.Printf("[INFO] Auth manager initialized with %d token slot(s)", len(slots))

	// Start background refresh goroutine
	go mgr.backgroundRefreshLoop()

	return nil
}

// loadSavedSlot reads data/auth_slot_X.json and overrides the slot's tokens
// if the saved refresh_token is different from config (i.e. it was rotated by WorkOS).
func loadSavedSlot(slot *tokenSlot) {
	filePath := filepath.Join(".", "data", fmt.Sprintf("auth_slot_%d.json", slot.index))
	data, err := os.ReadFile(filePath)
	if err != nil {
		// No saved file — use config.yaml token (first run)
		return
	}

	var saved struct {
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
		LastUpdated  string `json:"last_updated"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		log.Printf("[WARN] Slot[%d] failed to parse saved token file, using config value: %v", slot.index, err)
		return
	}

	if saved.RefreshToken == "" {
		return
	}

	// Use the saved (newer) refresh token instead of config.yaml's (stale) one
	if saved.RefreshToken != slot.refreshToken {
		log.Printf("[INFO] Slot[%d] loaded saved refresh token from %s (rotated, config.yaml value is stale)",
			slot.index, filePath)
	} else {
		log.Printf("[INFO] Slot[%d] saved refresh token matches config.yaml", slot.index)
	}
	slot.refreshToken = saved.RefreshToken

	// Also restore access token so requests can work immediately before refresh completes
	if saved.AccessToken != "" {
		slot.accessToken = saved.AccessToken
		log.Printf("[INFO] Slot[%d] restored cached access token (will refresh shortly)", slot.index)
	}
}

// GetBearerToken returns a Bearer token for the request.
// sessionID is used for session binding — same session always uses the same token slot.
// If clientAuth is provided and no managed tokens exist, it falls back to client auth.
// Returns: token, slotIndex (-1 if not managed), error
func GetBearerToken(sessionID string, clientAuth string) (string, int, error) {
	if globalManager == nil {
		if clientAuth != "" {
			return clientAuth, -1, nil
		}
		return "", -1, fmt.Errorf("no auth configured")
	}
	return globalManager.getBearerToken(sessionID, clientAuth)
}

// MarkSlotDisabled marks a token slot as permanently disabled (e.g. 401 = account banned).
func MarkSlotDisabled(slotIndex int, reason string) {
	if globalManager == nil {
		return
	}
	globalManager.markDisabled(slotIndex, reason)
}

// UnbindSession removes session binding so the next request picks a new slot.
func UnbindSession(sessionID string) {
	if globalManager == nil || sessionID == "" {
		return
	}
	globalManager.sessions.Delete(sessionID)
}

// GetNextBearerToken gets a token from a DIFFERENT slot than excludeSlot.
// Used for retry after 401/403.
func GetNextBearerToken(sessionID string, clientAuth string, excludeSlot int) (string, int, error) {
	if globalManager == nil {
		if clientAuth != "" {
			return clientAuth, -1, nil
		}
		return "", -1, fmt.Errorf("no auth configured")
	}
	return globalManager.getNextBearerToken(sessionID, clientAuth, excludeSlot)
}

// ActiveSlotCount returns how many slots are still usable.
func ActiveSlotCount() int {
	if globalManager == nil {
		return 0
	}
	globalManager.mu.RLock()
	defer globalManager.mu.RUnlock()
	count := 0
	for _, s := range globalManager.slots {
		s.mu.RLock()
		if !s.disabled && s.accessToken != "" {
			count++
		}
		s.mu.RUnlock()
	}
	return count
}

func (m *Manager) getBearerToken(sessionID string, clientAuth string) (string, int, error) {
	m.mu.RLock()
	slotCount := len(m.slots)
	m.mu.RUnlock()

	if slotCount == 0 {
		if clientAuth != "" {
			return clientAuth, -1, nil
		}
		return "", -1, fmt.Errorf("no auth available")
	}

	slot := m.getSlotForSession(sessionID)
	if slot == nil {
		return "", -1, fmt.Errorf("no available token slot (all slots disabled or empty)")
	}

	slot.mu.RLock()
	token := slot.accessToken
	idx := slot.index
	slot.mu.RUnlock()

	if token == "" {
		return "", -1, fmt.Errorf("token slot[%d] has no access token", idx)
	}

	return "Bearer " + token, idx, nil
}

func (m *Manager) getNextBearerToken(sessionID string, clientAuth string, excludeSlot int) (string, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.slots) == 0 {
		if clientAuth != "" {
			return clientAuth, -1, nil
		}
		return "", -1, fmt.Errorf("no auth available")
	}

	// Round-robin but skip excludeSlot and disabled slots
	for i := 0; i < len(m.slots); i++ {
		idx := (m.rrIndex + uint64(i)) % uint64(len(m.slots))
		s := m.slots[idx]
		if s.index == excludeSlot {
			continue
		}
		s.mu.RLock()
		hasToken := s.accessToken != "" && !s.disabled
		s.mu.RUnlock()
		if hasToken {
			m.rrIndex = uint64(idx) + 1
			if sessionID != "" {
				m.sessions.Store(sessionID, int(idx))
			}
			token := ""
			s.mu.RLock()
			token = s.accessToken
			s.mu.RUnlock()
			return "Bearer " + token, int(idx), nil
		}
	}

	return "", -1, fmt.Errorf("no available token slot for retry (all disabled)")
}

// getSlotForSession returns a token slot for the session.
// If sessionID is known, return the bound slot (if still active).
// Otherwise, do round-robin and bind the session to a slot.
// Skips disabled slots.
func (m *Manager) getSlotForSession(sessionID string) *tokenSlot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.slots) == 0 {
		return nil
	}

	// Session binding: if we've seen this session before, reuse the same slot
	if sessionID != "" {
		if idx, ok := m.sessions.Load(sessionID); ok {
			slotIdx := idx.(int)
			if slotIdx < len(m.slots) {
				s := m.slots[slotIdx]
				s.mu.RLock()
				usable := s.accessToken != "" && !s.disabled
				s.mu.RUnlock()
				if usable {
					return s
				}
				// Slot is disabled or empty — unbind and fall through to round-robin
				m.sessions.Delete(sessionID)
			}
		}
	}

	// Round-robin: pick next available slot (skip disabled)
	startIdx := m.rrIndex % uint64(len(m.slots))
	for i := 0; i < len(m.slots); i++ {
		idx := (startIdx + uint64(i)) % uint64(len(m.slots))
		s := m.slots[idx]
		s.mu.RLock()
		usable := s.accessToken != "" && !s.disabled
		s.mu.RUnlock()
		if usable {
			m.rrIndex = uint64(idx) + 1
			// Bind this session to this slot
			if sessionID != "" {
				m.sessions.Store(sessionID, int(idx))
			}
			return s
		}
	}

	return nil
}

// markDisabled marks a slot as permanently disabled.
func (m *Manager) markDisabled(slotIndex int, reason string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if slotIndex < 0 || slotIndex >= len(m.slots) {
		return
	}

	s := m.slots[slotIndex]
	s.mu.Lock()
	if !s.disabled {
		s.disabled = true
		s.disabledReason = reason
		log.Printf("[WARN] ========================================")
		log.Printf("[WARN] Token slot[%d] DISABLED: %s", slotIndex, reason)
		log.Printf("[WARN] Remaining active slots: %d/%d", m.countActiveLocked()-1, len(m.slots))
		log.Printf("[WARN] ========================================")
	}
	s.mu.Unlock()

	// Unbind all sessions that were bound to this slot
	m.sessions.Range(func(key, value interface{}) bool {
		if value.(int) == slotIndex {
			m.sessions.Delete(key)
		}
		return true
	})
}

// countActiveLocked returns active (non-disabled, has token) slot count.
// Caller must hold m.mu.RLock.
func (m *Manager) countActiveLocked() int {
	count := 0
	for _, s := range m.slots {
		s.mu.RLock()
		if !s.disabled && s.accessToken != "" {
			count++
		}
		s.mu.RUnlock()
	}
	return count
}

// refreshSlot performs a token refresh for one slot.
func (m *Manager) refreshSlot(slot *tokenSlot) error {
	slot.mu.RLock()
	refreshToken := slot.refreshToken
	slot.mu.RUnlock()

	if refreshToken == "" {
		return fmt.Errorf("empty refresh token")
	}

	log.Printf("[INFO] Refreshing token slot[%d]...", slot.index)

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)

	resp, err := m.httpClient.PostForm(refreshURL, form)
	if err != nil {
		return fmt.Errorf("refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body []byte
		body = make([]byte, 1024)
		n, _ := resp.Body.Read(body)
		return fmt.Errorf("refresh failed HTTP %d: %s", resp.StatusCode, string(body[:n]))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		User         struct {
			Email     string `json:"email"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
			ID        string `json:"id"`
		} `json:"user"`
		OrganizationID string `json:"organization_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}

	slot.mu.Lock()
	slot.accessToken = result.AccessToken
	slot.refreshToken = result.RefreshToken
	slot.lastRefresh = time.Now()
	slot.mu.Unlock()

	if result.User.Email != "" {
		log.Printf("[INFO] Slot[%d] authenticated as: %s (%s %s)",
			slot.index, result.User.Email, result.User.FirstName, result.User.LastName)
	}
	log.Printf("[INFO] Slot[%d] new refresh key: %s...", slot.index, truncate(result.RefreshToken, 20))
	log.Printf("[INFO] Slot[%d] refreshed successfully", slot.index)

	// Save tokens
	m.saveSlotTokens(slot, result.AccessToken, result.RefreshToken)

	return nil
}

func (m *Manager) saveSlotTokens(slot *tokenSlot, accessToken, refreshToken string) {
	authData := map[string]interface{}{
		"slot_index":    slot.index,
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"last_updated":  time.Now().Format(time.RFC3339),
	}
	dir := filepath.Join(".", "data")
	os.MkdirAll(dir, 0o755)
	filePath := filepath.Join(dir, fmt.Sprintf("auth_slot_%d.json", slot.index))
	data, _ := json.MarshalIndent(authData, "", "  ")
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		log.Printf("[ERROR] Failed to save slot[%d] tokens: %v", slot.index, err)
	}
}

// backgroundRefreshLoop refreshes all tokens periodically.
func (m *Manager) backgroundRefreshLoop() {
	c := config.Get()
	interval := time.Duration(c.RefreshIntervalHours) * time.Hour
	if interval < time.Hour {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		log.Println("[INFO] Scheduled token refresh starting...")
		m.mu.RLock()
		slots := m.slots
		m.mu.RUnlock()

		var wg sync.WaitGroup
		for _, slot := range slots {
			slot.mu.RLock()
			isDisabled := slot.disabled
			slot.mu.RUnlock()
			if isDisabled {
				continue // skip disabled slots
			}
			wg.Add(1)
			go func(s *tokenSlot) {
				defer wg.Done()
				if err := m.refreshSlot(s); err != nil {
					log.Printf("[ERROR] Background refresh slot[%d] failed: %v", s.index, err)
				}
			}(slot)
		}
		wg.Wait()
		log.Println("[INFO] Scheduled token refresh completed")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---- Management API for Web UI ----

// SlotInfo is the public view of a token slot for the Web UI.
type SlotInfo struct {
	Index          int    `json:"index"`
	RefreshToken   string `json:"refresh_token"`   // masked
	AccessToken    string `json:"access_token"`     // masked
	LastRefresh    string `json:"last_refresh"`
	Disabled       bool   `json:"disabled"`
	DisabledReason string `json:"disabled_reason"`
	Status         string `json:"status"` // "active", "disabled", "no_token"
}

func maskToken(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

// ListSlots returns info about all token slots.
func ListSlots() []SlotInfo {
	if globalManager == nil {
		return nil
	}
	globalManager.mu.RLock()
	defer globalManager.mu.RUnlock()

	result := make([]SlotInfo, len(globalManager.slots))
	for i, s := range globalManager.slots {
		s.mu.RLock()
		info := SlotInfo{
			Index:          s.index,
			RefreshToken:   maskToken(s.refreshToken),
			AccessToken:    maskToken(s.accessToken),
			Disabled:       s.disabled,
			DisabledReason: s.disabledReason,
		}
		if !s.lastRefresh.IsZero() {
			info.LastRefresh = s.lastRefresh.Format("2006-01-02 15:04:05")
		}
		if s.disabled {
			info.Status = "disabled"
		} else if s.accessToken != "" {
			info.Status = "active"
		} else {
			info.Status = "no_token"
		}
		s.mu.RUnlock()
		result[i] = info
	}
	return result
}

// AddSlot adds a new refresh key slot and immediately refreshes it.
func AddSlot(refreshKey string) (int, error) {
	if globalManager == nil {
		return -1, fmt.Errorf("auth manager not initialized")
	}

	globalManager.mu.Lock()
	idx := len(globalManager.slots)
	slot := &tokenSlot{
		refreshToken: refreshKey,
		index:        idx,
	}
	globalManager.slots = append(globalManager.slots, slot)
	globalManager.mu.Unlock()

	log.Printf("[INFO] Added new token slot[%d], refreshing...", idx)
	if err := globalManager.refreshSlot(slot); err != nil {
		return idx, fmt.Errorf("slot added but refresh failed: %w", err)
	}
	return idx, nil
}

// RemoveSlot removes a token slot by index.
func RemoveSlot(index int) error {
	if globalManager == nil {
		return fmt.Errorf("auth manager not initialized")
	}

	globalManager.mu.Lock()
	defer globalManager.mu.Unlock()

	if index < 0 || index >= len(globalManager.slots) {
		return fmt.Errorf("slot index %d out of range", index)
	}

	// Remove slot
	globalManager.slots = append(globalManager.slots[:index], globalManager.slots[index+1:]...)

	// Re-index remaining slots
	for i, s := range globalManager.slots {
		s.mu.Lock()
		s.index = i
		s.mu.Unlock()
	}

	// Clean up sessions bound to removed or shifted slots
	globalManager.sessions.Range(func(key, value interface{}) bool {
		slotIdx := value.(int)
		if slotIdx >= len(globalManager.slots) {
			globalManager.sessions.Delete(key)
		}
		return true
	})

	// Remove saved token file
	filePath := filepath.Join(".", "data", fmt.Sprintf("auth_slot_%d.json", index))
	os.Remove(filePath)

	log.Printf("[INFO] Removed token slot[%d], %d slots remaining", index, len(globalManager.slots))
	return nil
}

// ReplaceSlot replaces a slot's refresh key and re-enables it. Used to fix 401'd slots.
func ReplaceSlot(index int, newRefreshKey string) error {
	if globalManager == nil {
		return fmt.Errorf("auth manager not initialized")
	}

	globalManager.mu.RLock()
	if index < 0 || index >= len(globalManager.slots) {
		globalManager.mu.RUnlock()
		return fmt.Errorf("slot index %d out of range", index)
	}
	slot := globalManager.slots[index]
	globalManager.mu.RUnlock()

	slot.mu.Lock()
	slot.refreshToken = newRefreshKey
	slot.accessToken = ""
	slot.disabled = false
	slot.disabledReason = ""
	slot.mu.Unlock()

	log.Printf("[INFO] Replaced token slot[%d], refreshing...", index)
	if err := globalManager.refreshSlot(slot); err != nil {
		return fmt.Errorf("replaced but refresh failed: %w", err)
	}
	return nil
}

// ForceRefreshSlot forces a refresh on a specific slot.
func ForceRefreshSlot(index int) error {
	if globalManager == nil {
		return fmt.Errorf("auth manager not initialized")
	}

	globalManager.mu.RLock()
	if index < 0 || index >= len(globalManager.slots) {
		globalManager.mu.RUnlock()
		return fmt.Errorf("slot index %d out of range", index)
	}
	slot := globalManager.slots[index]
	globalManager.mu.RUnlock()

	return globalManager.refreshSlot(slot)
}
