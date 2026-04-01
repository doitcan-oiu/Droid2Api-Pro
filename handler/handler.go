package handler

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
	"sync"
	"time"

	"droid2api/auth"
	"droid2api/config"
	"droid2api/proxy"
	"droid2api/transformer"
)

// httpClient is a shared client with reasonable defaults for high concurrency.
var httpClient = &http.Client{
	Timeout: 10 * time.Minute,
	Transport: &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     120 * time.Second,
	},
}

// ---- Request/Response file logger ----

var (
	reqLogMu   sync.Mutex
	reqLogFile *os.File
	reqLogSeq  int64
)

func initReqLog() {
	if reqLogFile != nil {
		return
	}
	dir := filepath.Join(config.BaseDir(), "logs")
	os.MkdirAll(dir, 0o755)
	name := fmt.Sprintf("requests_%s.log", time.Now().Format("2006-01-02"))
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("[ERROR] Failed to open request log file: %v", err)
		return
	}
	reqLogFile = f
}

// logUpstreamRequest logs the full upstream request (URL, headers, body) and response status to a file.
func logUpstreamRequest(route string, method string, url string, headers map[string]string, body []byte, respStatus int, respBody string) {
	reqLogMu.Lock()
	defer reqLogMu.Unlock()

	initReqLog()
	if reqLogFile == nil {
		return
	}

	reqLogSeq++
	ts := time.Now().Format("2006-01-02 15:04:05.000")

	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("\n%s %s\n", strings.Repeat("=", 40), strings.Repeat("=", 39)))
	buf.WriteString(fmt.Sprintf("[#%d] %s | Route: %s\n", reqLogSeq, ts, route))
	buf.WriteString(fmt.Sprintf(">> %s %s\n", method, url))

	// Headers (mask authorization)
	buf.WriteString(">> Headers:\n")
	for k, v := range headers {
		display := v
		lk := strings.ToLower(k)
		if lk == "authorization" && len(v) > 20 {
			display = v[:20] + "..."
		}
		buf.WriteString(fmt.Sprintf("   %s: %s\n", k, display))
	}

	// Body (pretty-print JSON)
	buf.WriteString(">> Body:\n")
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "   ", "  ") == nil {
		buf.WriteString("   ")
		buf.Write(pretty.Bytes())
		buf.WriteString("\n")
	} else {
		buf.WriteString("   ")
		buf.Write(body)
		buf.WriteString("\n")
	}

	// Response
	buf.WriteString(fmt.Sprintf("<< Response Status: %d\n", respStatus))
	if respBody != "" {
		buf.WriteString(fmt.Sprintf("<< Response Body: %s\n", respBody))
	}
	buf.WriteString(strings.Repeat("=", 80))
	buf.WriteString("\n")

	reqLogFile.Write(buf.Bytes())
	reqLogFile.Sync()
}

// getAuthHeader retrieves the bearer token with session binding.
// Returns: token, slotIndex, error
func getAuthHeader(r *http.Request) (string, int, error) {
	sessionID := r.Header.Get("X-Session-Id")
	clientAuth := r.Header.Get("Authorization")
	if xKey := r.Header.Get("X-Api-Key"); xKey != "" && clientAuth == "" {
		clientAuth = "Bearer " + xKey
	}
	return auth.GetBearerToken(sessionID, clientAuth)
}

// forwardRequest sends a request to the upstream endpoint and returns the response.
func forwardRequest(method, targetURL string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Use proxy transport if available
	client := httpClient
	if transport := proxy.GetTransport(targetURL); transport != nil {
		client = &http.Client{
			Timeout:   10 * time.Minute,
			Transport: transport,
		}
	}

	return client.Do(req)
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string, details ...string) {
	resp := map[string]interface{}{"error": msg}
	if len(details) > 0 {
		resp["details"] = details[0]
	}
	writeJSON(w, status, resp)
}

// readBody reads the entire request body into a byte slice.
func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

// ---- Route handlers ----

// HandleModels handles GET /v1/models.
func HandleModels(w http.ResponseWriter, r *http.Request) {
	log.Println("[INFO] GET /v1/models")
	c := config.Get()

	models := make([]map[string]interface{}, 0, len(c.Models))
	for _, m := range c.Models {
		models = append(models, map[string]interface{}{
			"id":         m.ID,
			"object":     "model",
			"created":    time.Now().Unix(),
			"owned_by":   m.Type,
			"permission": []interface{}{},
			"root":       m.ID,
			"parent":     nil,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

// HandleChatCompletions handles POST /v1/chat/completions — with format transformation.
func HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	log.Println("[INFO] POST /v1/chat/completions")

	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	rawModelID, _ := req["model"].(string)
	if rawModelID == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	modelID := config.RedirectModel(rawModelID)
	req["model"] = modelID

	model := config.GetModelByID(modelID)
	if model == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Model %s not found", modelID))
		return
	}

	endpoint := config.GetEndpointByType(model.Type)
	if endpoint == nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Endpoint type %s not found", model.Type))
		return
	}

	sessionID := r.Header.Get("X-Session-Id")
	provider := config.GetModelProvider(modelID)
	isStreaming, _ := req["stream"].(bool)

	// Retry loop: try different token slots on 401/403
	maxRetries := auth.ActiveSlotCount() + 2 // extra retries for 500/502/503
	if maxRetries < 1 {
		maxRetries = 1
	}
	excludeSlot := -1
	var lastErrBody string
	var lastStatus int

	for attempt := 0; attempt < maxRetries; attempt++ {
		var authHeader string
		var slotIndex int

		if attempt == 0 {
			authHeader, slotIndex, err = getAuthHeader(r)
		} else {
			clientAuth := r.Header.Get("Authorization")
			if xKey := r.Header.Get("X-Api-Key"); xKey != "" && clientAuth == "" {
				clientAuth = "Bearer " + xKey
			}
			authHeader, slotIndex, err = auth.GetNextBearerToken(sessionID, clientAuth, excludeSlot)
		}
		if err != nil {
			if attempt == 0 {
				writeError(w, http.StatusInternalServerError, "API key not available", err.Error())
				return
			}
			break // no more slots
		}

		// Transform request based on type
		var transformed map[string]interface{}
		var headers map[string]string

		switch model.Type {
		case "anthropic":
			transformed = transformer.TransformToAnthropic(req)
			headers = transformer.GetAnthropicHeaders(authHeader, r.Header, isStreaming, modelID, provider)
		case "openai":
			transformed = transformer.TransformToOpenAI(req)
			headers = transformer.GetOpenAIHeaders(authHeader, r.Header, provider)
		case "google":
			transformed = transformer.TransformToGoogle(req)
			headers = transformer.GetGoogleHeaders(authHeader, r.Header, provider)
		case "common":
			transformed = transformer.TransformToCommon(req)
			headers = transformer.GetCommonHeaders(authHeader, r.Header, provider)
		default:
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("Unknown endpoint type: %s", model.Type))
			return
		}

		transformedBody, _ := json.Marshal(transformed)
		log.Printf("[REQUEST] POST %s (slot[%d], attempt %d/%d)", endpoint.BaseURL, slotIndex, attempt+1, maxRetries)

		resp, err := forwardRequest("POST", endpoint.BaseURL, headers, transformedBody)
		if err != nil {
			logUpstreamRequest("/v1/chat/completions", "POST", endpoint.BaseURL, headers, transformedBody, 0, "ERROR: "+err.Error())
			log.Printf("[ERROR] Upstream request failed: %v", err)
			writeError(w, http.StatusBadGateway, "upstream request failed", err.Error())
			return
		}

		log.Printf("[INFO] Response status: %d", resp.StatusCode)

		// Check for 401 — mark slot disabled and retry with next token
		if resp.StatusCode == 401 && slotIndex >= 0 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErrBody = string(errBody)
			lastStatus = resp.StatusCode

			logUpstreamRequest("/v1/chat/completions", "POST", endpoint.BaseURL, headers, transformedBody, 401, lastErrBody)
			log.Printf("[WARN] Slot[%d] returned 401 (account banned), disabling and retrying...", slotIndex)
			auth.MarkSlotDisabled(slotIndex, "HTTP 401 - Account banned")
			auth.UnbindSession(sessionID)
			excludeSlot = slotIndex
			continue
		}

		// 403 — not a token issue, return directly
		if resp.StatusCode == 403 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			logUpstreamRequest("/v1/chat/completions", "POST", endpoint.BaseURL, headers, transformedBody, 403, string(errBody))
			log.Printf("[WARN] Upstream returned 403")
			writeError(w, http.StatusForbidden, "当前还暂未适配该接口")
			return
		}

		// 500/502/503/429 — upstream server error, retry after delay
		if resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 429 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErrBody = string(errBody)
			lastStatus = resp.StatusCode
			logUpstreamRequest("/v1/chat/completions", "POST", endpoint.BaseURL, headers, transformedBody, resp.StatusCode, lastErrBody)
			log.Printf("[WARN] Upstream returned %d, retrying after delay (attempt %d/%d)...", resp.StatusCode, attempt+1, maxRetries)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			logUpstreamRequest("/v1/chat/completions", "POST", endpoint.BaseURL, headers, transformedBody, resp.StatusCode, string(errBody))
			writeError(w, resp.StatusCode, fmt.Sprintf("Endpoint returned %d", resp.StatusCode), string(errBody))
			return
		}

		// Success — log and send response
		logUpstreamRequest("/v1/chat/completions", "POST", endpoint.BaseURL, headers, transformedBody, resp.StatusCode, "OK (streaming="+fmt.Sprint(isStreaming)+")")
		if isStreaming {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			flusher, ok := w.(http.Flusher)
			flush := func() {
				if ok {
					flusher.Flush()
				}
			}

			var streamErr error
			switch model.Type {
			case "common":
				defer resp.Body.Close()
				buf := make([]byte, 32*1024)
				for {
					n, err := resp.Body.Read(buf)
					if n > 0 {
						w.Write(buf[:n])
						flush()
					}
					if err != nil {
						break
					}
				}
			case "anthropic":
				streamErr = transformer.TransformAnthropicStream(resp.Body, modelID, w, flush)
			case "openai":
				streamErr = transformer.TransformOpenAIStream(resp.Body, modelID, w, flush)
			case "google":
				streamErr = transformer.TransformGoogleStream(resp.Body, modelID, w, flush)
			}

			if streamErr != nil {
				log.Printf("[ERROR] Stream error: %v", streamErr)
			} else {
				log.Println("[INFO] Stream completed")
			}
		} else {
			defer resp.Body.Close()
			var data map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to decode upstream response")
				return
			}

			switch model.Type {
			case "openai":
				data = transformer.ConvertResponseToChatCompletion(data)
			case "google":
				data = transformer.ConvertGoogleResponseToChatCompletion(data, modelID)
			}
			writeJSON(w, http.StatusOK, data)
		}
		return // success, done
	}

	// All retries exhausted
	log.Printf("[ERROR] All token slots exhausted after retries")
	writeError(w, lastStatus, fmt.Sprintf("All token slots failed (last: %d)", lastStatus), lastErrBody)
}

// HandleDirectResponses handles POST /v1/responses — direct forward for OpenAI type.
func HandleDirectResponses(w http.ResponseWriter, r *http.Request) {
	log.Println("[INFO] POST /v1/responses")
	handleDirect(w, r, "openai", "/v1/responses")
}

// HandleDirectMessages handles POST /v1/messages — direct forward for Anthropic type.
func HandleDirectMessages(w http.ResponseWriter, r *http.Request) {
	log.Println("[INFO] POST /v1/messages")
	handleDirect(w, r, "anthropic", "/v1/messages")
}

// HandleDirectGenerate handles POST /v1/generate — direct forward for Google type.
func HandleDirectGenerate(w http.ResponseWriter, r *http.Request) {
	log.Println("[INFO] POST /v1/generate")
	handleDirect(w, r, "google", "/v1/generate")
}

// handleDirect is the common logic for direct-forward endpoints.
// Supports 401/403 retry with token slot failover.
func handleDirect(w http.ResponseWriter, r *http.Request, expectedType string, endpointPath string) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	rawModelID, _ := req["model"].(string)
	if rawModelID == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	modelID := config.RedirectModel(rawModelID)
	req["model"] = modelID

	model := config.GetModelByID(modelID)
	if model == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Model %s not found", modelID))
		return
	}

	if model.Type != expectedType {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("%s only supports %s type, model %s is %s type", endpointPath, expectedType, modelID, model.Type))
		return
	}

	endpoint := config.GetEndpointByType(model.Type)
	if endpoint == nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Endpoint type %s not found", model.Type))
		return
	}

	sessionID := r.Header.Get("X-Session-Id")
	provider := config.GetModelProvider(modelID)
	isStreaming, _ := req["stream"].(bool)
	c := config.Get()

	maxRetries := auth.ActiveSlotCount() + 2 // extra retries for 500/502/503
	if maxRetries < 1 {
		maxRetries = 1
	}
	excludeSlot := -1
	var lastErrBody string
	var lastStatus int

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Get a fresh copy of req for each attempt (system prompt injection modifies it)
		var attemptReq map[string]interface{}
		json.Unmarshal(body, &attemptReq)
		attemptReq["model"] = modelID

		var authHeader string
		var slotIndex int

		if attempt == 0 {
			authHeader, slotIndex, err = getAuthHeader(r)
		} else {
			clientAuth := r.Header.Get("Authorization")
			if xKey := r.Header.Get("X-Api-Key"); xKey != "" && clientAuth == "" {
				clientAuth = "Bearer " + xKey
			}
			authHeader, slotIndex, err = auth.GetNextBearerToken(sessionID, clientAuth, excludeSlot)
		}
		if err != nil {
			if attempt == 0 {
				writeError(w, http.StatusInternalServerError, "API key not available", err.Error())
				return
			}
			break
		}

		// Build headers and inject system prompt based on type
		var headers map[string]string
		switch expectedType {
		case "anthropic":
			headers = transformer.GetAnthropicHeaders(authHeader, r.Header, isStreaming, modelID, provider)
			if c.SystemPrompt != "" {
				injectAnthropicSystemPrompt(attemptReq, c.SystemPrompt)
			}
			handleAnthropicThinking(attemptReq, modelID)
			sanitizeForFactory(attemptReq, headers)
		case "openai":
			headers = transformer.GetOpenAIHeaders(authHeader, r.Header, provider)
			if c.SystemPrompt != "" {
				injectOpenAISystemPrompt(attemptReq, c.SystemPrompt)
			}
			handleOpenAIReasoning(attemptReq, modelID)
		case "google":
			headers = transformer.GetGoogleHeaders(authHeader, r.Header, provider)
			if c.SystemPrompt != "" {
				injectGoogleSystemPrompt(attemptReq, c.SystemPrompt)
			}
			handleGoogleThinking(attemptReq, modelID)
		}

		targetURL := endpoint.BaseURL
		reqBody, _ := json.Marshal(attemptReq)
		log.Printf("[REQUEST] POST %s (slot[%d], attempt %d/%d)", targetURL, slotIndex, attempt+1, maxRetries)

		resp, err := forwardRequest("POST", targetURL, headers, reqBody)
		if err != nil {
			logUpstreamRequest(endpointPath, "POST", targetURL, headers, reqBody, 0, "ERROR: "+err.Error())
			writeError(w, http.StatusBadGateway, "upstream request failed", err.Error())
			return
		}

		log.Printf("[INFO] Response status: %d", resp.StatusCode)

		// Check for 401 — mark slot disabled and retry with next token
		if resp.StatusCode == 401 && slotIndex >= 0 {
			errBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErrBody = string(errBytes)
			lastStatus = resp.StatusCode

			logUpstreamRequest(endpointPath, "POST", targetURL, headers, reqBody, 401, lastErrBody)
			log.Printf("[WARN] Slot[%d] returned 401 (account banned), disabling and retrying...", slotIndex)
			auth.MarkSlotDisabled(slotIndex, "HTTP 401 - Account banned")
			auth.UnbindSession(sessionID)
			excludeSlot = slotIndex
			continue
		}

		// 403 — not a token issue, return directly
		if resp.StatusCode == 403 {
			errBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			logUpstreamRequest(endpointPath, "POST", targetURL, headers, reqBody, 403, string(errBytes))
			log.Printf("[WARN] Upstream returned 403")
			writeError(w, http.StatusForbidden, "当前还暂未适配该接口")
			return
		}

		// 500/502/503/429 — upstream server error, retry with same slot after delay
		if resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 429 {
			errBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErrBody = string(errBytes)
			lastStatus = resp.StatusCode
			logUpstreamRequest(endpointPath, "POST", targetURL, headers, reqBody, resp.StatusCode, lastErrBody)
			log.Printf("[WARN] Upstream returned %d, retrying after delay (attempt %d/%d)...", resp.StatusCode, attempt+1, maxRetries)
			time.Sleep(time.Duration(attempt+1) * time.Second) // exponential-ish backoff: 1s, 2s, 3s...
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			logUpstreamRequest(endpointPath, "POST", targetURL, headers, reqBody, resp.StatusCode, string(errBytes))
			writeError(w, resp.StatusCode, fmt.Sprintf("Endpoint returned %d", resp.StatusCode), string(errBytes))
			return
		}

		// Success — log and send response
		logUpstreamRequest(endpointPath, "POST", targetURL, headers, reqBody, resp.StatusCode, "OK (streaming="+fmt.Sprint(isStreaming)+")")
		if isStreaming {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			defer resp.Body.Close()
			flusher, ok := w.(http.Flusher)
			buf := make([]byte, 32*1024)
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
					if ok {
						flusher.Flush()
					}
				}
				if readErr != nil {
					break
				}
			}
			log.Println("[INFO] Stream forwarded successfully")
		} else {
			defer resp.Body.Close()
			var data map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to decode upstream response")
				return
			}
			writeJSON(w, http.StatusOK, data)
		}
		return // success
	}

	// All retries exhausted
	log.Printf("[ERROR] All token slots exhausted after retries")
	writeError(w, lastStatus, fmt.Sprintf("All token slots failed (last: %d)", lastStatus), lastErrBody)
}

// HandleCountTokens handles POST /v1/messages/count_tokens.
// Supports 401/403 retry with token slot failover.
func HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	log.Println("[INFO] POST /v1/messages/count_tokens")

	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	rawModelID, _ := req["model"].(string)
	if rawModelID == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	modelID := config.RedirectModel(rawModelID)
	req["model"] = modelID

	model := config.GetModelByID(modelID)
	if model == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Model %s not found", modelID))
		return
	}
	if model.Type != "anthropic" {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("/v1/messages/count_tokens only supports anthropic type, model %s is %s type", modelID, model.Type))
		return
	}

	endpoint := config.GetEndpointByType("anthropic")
	if endpoint == nil {
		writeError(w, http.StatusInternalServerError, "Endpoint type anthropic not found")
		return
	}

	sessionID := r.Header.Get("X-Session-Id")
	provider := config.GetModelProvider(modelID)
	countURL := strings.Replace(endpoint.BaseURL, "/v1/messages", "/v1/messages/count_tokens", 1)

	maxRetries := auth.ActiveSlotCount() + 2 // extra retries for 500/502/503
	if maxRetries < 1 {
		maxRetries = 1
	}
	excludeSlot := -1
	var lastErrBody string
	var lastStatus int

	for attempt := 0; attempt < maxRetries; attempt++ {
		var authHeader string
		var slotIndex int

		if attempt == 0 {
			authHeader, slotIndex, err = getAuthHeader(r)
		} else {
			clientAuth := r.Header.Get("Authorization")
			if xKey := r.Header.Get("X-Api-Key"); xKey != "" && clientAuth == "" {
				clientAuth = "Bearer " + xKey
			}
			authHeader, slotIndex, err = auth.GetNextBearerToken(sessionID, clientAuth, excludeSlot)
		}
		if err != nil {
			if attempt == 0 {
				writeError(w, http.StatusInternalServerError, "API key not available", err.Error())
				return
			}
			break
		}

		headers := transformer.GetAnthropicHeaders(authHeader, r.Header, false, modelID, provider)
		reqBody, _ := json.Marshal(req)
		log.Printf("[REQUEST] POST %s (slot[%d], attempt %d/%d)", countURL, slotIndex, attempt+1, maxRetries)

		resp, err := forwardRequest("POST", countURL, headers, reqBody)
		if err != nil {
			logUpstreamRequest("/v1/messages/count_tokens", "POST", countURL, headers, reqBody, 0, "ERROR: "+err.Error())
			writeError(w, http.StatusBadGateway, "upstream request failed", err.Error())
			return
		}

		log.Printf("[INFO] Response status: %d", resp.StatusCode)

		if resp.StatusCode == 401 && slotIndex >= 0 {
			errBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErrBody = string(errBytes)
			lastStatus = resp.StatusCode

			logUpstreamRequest("/v1/messages/count_tokens", "POST", countURL, headers, reqBody, 401, lastErrBody)
			log.Printf("[WARN] Slot[%d] returned 401 (account banned), disabling and retrying...", slotIndex)
			auth.MarkSlotDisabled(slotIndex, "HTTP 401 - Account banned")
			auth.UnbindSession(sessionID)
			excludeSlot = slotIndex
			continue
		}

		// 403 — not a token issue, return directly
		if resp.StatusCode == 403 {
			errBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			logUpstreamRequest("/v1/messages/count_tokens", "POST", countURL, headers, reqBody, 403, string(errBytes))
			log.Printf("[WARN] Upstream returned 403")
			writeError(w, http.StatusForbidden, "当前还暂未适配该接口")
			return
		}

		// 500/502/503/429 — upstream server error, retry after delay
		if resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 429 {
			errBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErrBody = string(errBytes)
			lastStatus = resp.StatusCode
			logUpstreamRequest("/v1/messages/count_tokens", "POST", countURL, headers, reqBody, resp.StatusCode, string(errBytes))
			log.Printf("[WARN] Upstream returned %d, retrying after delay (attempt %d/%d)...", resp.StatusCode, attempt+1, maxRetries)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			logUpstreamRequest("/v1/messages/count_tokens", "POST", countURL, headers, reqBody, resp.StatusCode, string(errBytes))
			writeError(w, resp.StatusCode, fmt.Sprintf("Endpoint returned %d", resp.StatusCode), string(errBytes))
			return
		}

		logUpstreamRequest("/v1/messages/count_tokens", "POST", countURL, headers, reqBody, resp.StatusCode, "OK")
		defer resp.Body.Close()
		var data map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to decode response")
			return
		}
		writeJSON(w, http.StatusOK, data)
		return
	}

	log.Printf("[ERROR] All token slots exhausted after retries")
	writeError(w, lastStatus, fmt.Sprintf("All token slots failed (last: %d)", lastStatus), lastErrBody)
}

// ---- System prompt injection helpers ----

// droidIdentity is the exact identity string used for dedup after replacement.
const droidIdentity = "You are Droid, an AI software engineering agent built by Factory."

// systemPartsToRemove — system parts containing these strings are dropped entirely.
// They are client-injected metadata that Factory does not accept.
var systemPartsToRemove = []string{
	"x-anthropic-billing-header",
}

func injectAnthropicSystemPrompt(req map[string]interface{}, sysPrompt string) {
	existing, ok := req["system"].([]interface{})
	if !ok {
		req["system"] = []interface{}{map[string]interface{}{"type": "text", "text": sysPrompt}}
		return
	}

	// 1. Filter and clean system parts
	var cleaned []interface{}
	for _, raw := range existing {
		part, ok := raw.(map[string]interface{})
		if !ok {
			cleaned = append(cleaned, raw)
			continue
		}
		text, _ := part["text"].(string)
		if text == "" {
			cleaned = append(cleaned, raw)
			continue
		}

		// Drop parts that contain client-injected metadata
		shouldDrop := false
		for _, pattern := range systemPartsToRemove {
			if strings.Contains(text, pattern) {
				log.Printf("[INFO] Dropped system part containing '%s'", pattern)
				shouldDrop = true
				break
			}
		}
		if shouldDrop {
			continue
		}

		// Replace conflicting identity
		newText := transformer.CleanIdentityText(text)
		trimmed := strings.TrimSpace(newText)

		// Drop parts that became Droid-identity-only after replacement (duplicates)
		if trimmed == droidIdentity {
			log.Printf("[INFO] Dropped duplicate Droid-identity-only system part")
			continue
		}

		if newText != text {
			part["text"] = newText
		}
		cleaned = append(cleaned, part)
	}

	// 2. Always prepend our Droid prompt as system[0]
	result := make([]interface{}, 0, len(cleaned)+1)
	result = append(result, map[string]interface{}{"type": "text", "text": sysPrompt})
	result = append(result, cleaned...)

	req["system"] = result
}

func injectOpenAISystemPrompt(req map[string]interface{}, sysPrompt string) {
	if instructions, ok := req["instructions"].(string); ok {
		req["instructions"] = sysPrompt + transformer.CleanIdentityText(instructions)
	} else {
		req["instructions"] = sysPrompt
	}
}

func injectGoogleSystemPrompt(req map[string]interface{}, sysPrompt string) {
	if si, ok := req["systemInstruction"].(map[string]interface{}); ok {
		if parts, ok := si["parts"].([]interface{}); ok {
			for i, raw := range parts {
				part, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				text, _ := part["text"].(string)
				cleaned := transformer.CleanIdentityText(text)
				if cleaned != text {
					part["text"] = cleaned
					parts[i] = part
				}
			}
			newParts := append([]interface{}{map[string]interface{}{"text": sysPrompt}}, parts...)
			si["parts"] = newParts
		}
	} else {
		req["systemInstruction"] = map[string]interface{}{
			"parts": []interface{}{map[string]interface{}{"text": sysPrompt}},
		}
	}
}

// ---- Reasoning/Thinking helpers for direct endpoints ----

func handleAnthropicThinking(req map[string]interface{}, modelID string) {
	level := config.GetModelReasoning(modelID)
	switch level {
	case "auto":
		// preserve
	case "low", "medium", "high", "xhigh":
		budgets := map[string]int{"low": 4096, "medium": 12288, "high": 24576, "xhigh": 40960}
		req["thinking"] = map[string]interface{}{
			"type":          "enabled",
			"budget_tokens": budgets[level],
		}
	default:
		delete(req, "thinking")
	}
}

func handleOpenAIReasoning(req map[string]interface{}, modelID string) {
	level := config.GetModelReasoning(modelID)
	switch level {
	case "auto":
		// preserve
	case "low", "medium", "high", "xhigh":
		req["reasoning"] = map[string]interface{}{
			"effort":  level,
			"summary": "auto",
		}
	default:
		delete(req, "reasoning")
	}
}

func handleGoogleThinking(req map[string]interface{}, modelID string) {
	level := config.GetModelReasoning(modelID)
	switch level {
	case "auto":
		// preserve
	case "low", "medium", "high":
		levelMap := map[string]string{"low": "LOW", "medium": "MEDIUM", "high": "HIGH"}
		genCfg, ok := req["generationConfig"].(map[string]interface{})
		if !ok {
			genCfg = map[string]interface{}{}
		}
		genCfg["thinkingConfig"] = map[string]interface{}{"thinkingLevel": levelMap[level]}
		req["generationConfig"] = genCfg
	default:
		if genCfg, ok := req["generationConfig"].(map[string]interface{}); ok {
			delete(genCfg, "thinkingConfig")
		}
	}
}

// ---- Factory API sanitization ----

// betaPrefixesToRemove — anthropic-beta values starting with these prefixes are dropped.
var betaPrefixesToRemove = []string{
	"claude-code-",
	"context-1m-",
}

// bodyFieldsToRemove — request body fields that Factory does not recognize.
var bodyFieldsToRemove = []string{
	"output_config",
	"metadata",
}

// sanitizeForFactory cleans request body and headers for Factory API compatibility.
func sanitizeForFactory(req map[string]interface{}, headers map[string]string) {
	// 1. Remove unsupported body fields
	for _, field := range bodyFieldsToRemove {
		if _, ok := req[field]; ok {
			delete(req, field)
			log.Printf("[INFO] Removed unsupported body field: %s", field)
		}
	}

	// 2. Clean anthropic-beta header
	if beta, ok := headers["anthropic-beta"]; ok {
		parts := strings.Split(beta, ",")
		var cleaned []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			shouldDrop := false
			for _, prefix := range betaPrefixesToRemove {
				if strings.HasPrefix(p, prefix) {
					log.Printf("[INFO] Removed unsupported anthropic-beta: %s", p)
					shouldDrop = true
					break
				}
			}
			if !shouldDrop {
				cleaned = append(cleaned, p)
			}
		}
		if len(cleaned) > 0 {
			headers["anthropic-beta"] = strings.Join(cleaned, ", ")
		} else {
			delete(headers, "anthropic-beta")
		}
	}
}
