package proxy

import (
	"log"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"

	"droid2api/config"
)

var (
	mu       sync.Mutex
	rrIndex  uint64
	lastSnap string
)

// GetTransport returns an *http.Transport configured with the next round-robin proxy,
// or nil if no proxy is configured.
func GetTransport(targetURL string) *http.Transport {
	c := config.Get()
	proxies := c.Proxies
	if len(proxies) == 0 {
		return nil
	}

	mu.Lock()
	defer mu.Unlock()

	for attempt := 0; attempt < len(proxies); attempt++ {
		idx := (atomic.LoadUint64(&rrIndex) + uint64(attempt)) % uint64(len(proxies))
		p := proxies[idx]
		if p.URL == "" {
			continue
		}

		proxyURL, err := url.Parse(p.URL)
		if err != nil {
			log.Printf("[ERROR] Invalid proxy URL %s: %v", p.URL, err)
			continue
		}

		atomic.StoreUint64(&rrIndex, idx+1)
		label := p.Name
		if label == "" {
			label = p.URL
		}
		log.Printf("[INFO] Using proxy %s for %s", label, targetURL)

		return &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
	}

	log.Println("[ERROR] All configured proxies failed")
	return nil
}
