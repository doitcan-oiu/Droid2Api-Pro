package useragent

import (
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"droid2api/config"
)

const (
	versionURL    = "https://downloads.factory.ai/factory-cli/LATEST"
	prefix        = "factory-cli"
	checkInterval = 1 * time.Hour
	retryInterval = 1 * time.Minute
	maxRetries    = 3
)

var (
	mu             sync.RWMutex
	currentVersion string
	updating       bool
	versionRegex   = regexp.MustCompile(`\d+\.\d+\.\d+`)
)

func getDefaultVersion() string {
	c := config.Get()
	ua := c.UserAgent
	if ua == "" {
		return "0.19.3"
	}
	m := versionRegex.FindString(ua)
	if m != "" {
		return m
	}
	return "0.19.3"
}

// GetCurrentUserAgent returns the current user-agent string.
func GetCurrentUserAgent() string {
	mu.RLock()
	v := currentVersion
	mu.RUnlock()
	if v == "" {
		return prefix + "/" + getDefaultVersion()
	}
	return prefix + "/" + v
}

func fetchLatestVersion() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(versionURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	version := strings.TrimSpace(string(body))
	if versionRegex.MatchString(version) {
		return version, nil
	}
	return "", nil
}

func updateVersionWithRetry(retryCount int) {
	mu.Lock()
	if updating {
		mu.Unlock()
		return
	}
	updating = true
	mu.Unlock()

	defer func() {
		mu.Lock()
		updating = false
		mu.Unlock()
	}()

	version, err := fetchLatestVersion()
	if err != nil {
		log.Printf("[ERROR] Failed to fetch latest version (attempt %d/%d): %v", retryCount+1, maxRetries, err)
		if retryCount < maxRetries-1 {
			log.Println("[INFO] Retrying in 1 minute...")
			time.AfterFunc(retryInterval, func() { updateVersionWithRetry(retryCount + 1) })
		}
		return
	}

	if version == "" {
		return
	}

	mu.Lock()
	old := currentVersion
	currentVersion = version
	mu.Unlock()

	if old != version {
		log.Printf("[INFO] User-Agent version updated: %s -> %s", old, version)
	} else {
		log.Printf("[INFO] User-Agent version is up to date: %s", version)
	}
}

// Initialize starts the user-agent version updater.
func Initialize() {
	mu.Lock()
	currentVersion = getDefaultVersion()
	mu.Unlock()

	log.Printf("[INFO] User-Agent updater initialized: %s/%s", prefix, currentVersion)

	// Fetch immediately
	go updateVersionWithRetry(0)

	// Hourly checks
	go func() {
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for range ticker.C {
			log.Println("[INFO] Running scheduled User-Agent version check...")
			updateVersionWithRetry(0)
		}
	}()
}
