package logger

import (
	"encoding/json"
	"log"

	"droid2api/config"
)

func Info(msg string, data ...any) {
	log.Printf("[INFO] %s", msg)
	if len(data) > 0 && config.Get() != nil && config.Get().DevMode {
		b, _ := json.MarshalIndent(data[0], "", "  ")
		log.Println(string(b))
	}
}

func Debug(msg string, data ...any) {
	if config.Get() == nil || !config.Get().DevMode {
		return
	}
	log.Printf("[DEBUG] %s", msg)
	if len(data) > 0 {
		b, _ := json.MarshalIndent(data[0], "", "  ")
		log.Println(string(b))
	}
}

func Error(msg string, err ...error) {
	if len(err) > 0 && err[0] != nil {
		log.Printf("[ERROR] %s: %v", msg, err[0])
	} else {
		log.Printf("[ERROR] %s", msg)
	}
}

func Request(method, url string, headers map[string]string, body any) {
	if config.Get() != nil && config.Get().DevMode {
		log.Println("\n" + "=" + repeated('=', 79))
		log.Printf("[REQUEST] %s %s", method, url)
		if headers != nil {
			b, _ := json.MarshalIndent(headers, "", "  ")
			log.Printf("[HEADERS] %s", string(b))
		}
		if body != nil {
			b, _ := json.MarshalIndent(body, "", "  ")
			log.Printf("[BODY] %s", string(b))
		}
		log.Println(repeated('=', 80))
	} else {
		log.Printf("[REQUEST] %s %s", method, url)
	}
}

func Response(status int, body any) {
	if config.Get() != nil && config.Get().DevMode {
		log.Println(repeated('-', 80))
		log.Printf("[RESPONSE] Status: %d", status)
		if body != nil {
			b, _ := json.MarshalIndent(body, "", "  ")
			log.Printf("[BODY] %s", string(b))
		}
		log.Println(repeated('-', 80))
	} else {
		log.Printf("[RESPONSE] Status: %d", status)
	}
}

func repeated(ch byte, n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = ch
	}
	return string(buf)
}
