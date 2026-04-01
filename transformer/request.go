package transformer

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"

	"droid2api/config"
)

// patternsToReplace are conflicting identity statements that must be rewritten.
var patternsToReplace = []string{
	"You are Claude Code",
	"You are Claude, made by Anthropic",
	"You are ChatGPT",
	"You are GPT",
}

// CleanIdentityText replaces conflicting identity sentences in text.
func CleanIdentityText(text string) string {
	for _, pattern := range patternsToReplace {
		if strings.Contains(text, pattern) {
			text = ReplaceIdentitySentence(text, pattern, "You are Droid, an AI software engineering agent built by Factory.")
			log.Printf("[INFO] Replaced conflicting identity in transformer: '%s'", pattern)
		}
	}
	return text
}

// ReplaceIdentitySentence finds the sentence containing 'pattern' and replaces it.
func ReplaceIdentitySentence(text, pattern, replacement string) string {
	idx := strings.Index(text, pattern)
	if idx < 0 {
		return text
	}
	start := idx
	for start > 0 && text[start-1] != '.' && text[start-1] != '\n' {
		start--
	}
	for start < idx && (text[start] == ' ' || text[start] == '\n') {
		start++
	}
	end := idx + len(pattern)
	for end < len(text) && text[end] != '.' && text[end] != '\n' {
		end++
	}
	if end < len(text) && text[end] == '.' {
		end++
	}
	return text[:start] + replacement + text[end:]
}

// ---- Generic types used across transformers ----

// GenericMessage is a generic message type for incoming OpenAI-format requests.
type GenericMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []ContentPart
}

// ContentPart is a multimodal content part.
type ContentPart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL interface{} `json:"image_url,omitempty"`
}

// parseContentParts extracts ContentParts from a message's content field.
func parseContentParts(content interface{}) (string, []ContentPart) {
	switch v := content.(type) {
	case string:
		return v, nil
	case []interface{}:
		var parts []ContentPart
		for _, item := range v {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			p := ContentPart{}
			if t, ok := m["type"].(string); ok {
				p.Type = t
			}
			if t, ok := m["text"].(string); ok {
				p.Text = t
			}
			if img, ok := m["image_url"]; ok {
				p.ImageURL = img
			}
			parts = append(parts, p)
		}
		return "", parts
	}
	return fmt.Sprintf("%v", content), nil
}

func generateUUID() string {
	const hex = "0123456789abcdef"
	uuid := []byte("xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx")
	for i, c := range uuid {
		if c == 'x' || c == 'y' {
			r := rand.Intn(16)
			if c == 'y' {
				r = (r & 0x3) | 0x8
			}
			uuid[i] = hex[r]
		}
	}
	return string(uuid)
}

// getReasoningBudget returns budget_tokens for anthropic thinking.
func getReasoningBudget(level string) int {
	switch level {
	case "low":
		return 4096
	case "medium":
		return 12288
	case "high":
		return 24576
	case "xhigh":
		return 40960
	}
	return 0
}

// ---- Anthropic request transformer ----

func TransformToAnthropic(req map[string]interface{}) map[string]interface{} {
	c := config.Get()
	modelID, _ := req["model"].(string)

	result := map[string]interface{}{
		"model":    modelID,
		"messages": []interface{}{},
	}

	if stream, ok := req["stream"]; ok {
		result["stream"] = stream
	}

	// max_tokens
	if mt, ok := req["max_tokens"]; ok {
		result["max_tokens"] = mt
	} else if mct, ok := req["max_completion_tokens"]; ok {
		result["max_tokens"] = mct
	} else {
		result["max_tokens"] = 4096
	}

	// Extract system messages and transform
	var systemParts []map[string]interface{}
	var messages []interface{}

	if msgs, ok := req["messages"].([]interface{}); ok {
		for _, raw := range msgs {
			msg, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)

			if role == "system" {
				text, parts := parseContentParts(msg["content"])
				if text != "" {
					systemParts = append(systemParts, map[string]interface{}{"type": "text", "text": CleanIdentityText(text)})
				}
				for _, p := range parts {
					if p.Type == "text" {
						systemParts = append(systemParts, map[string]interface{}{"type": "text", "text": CleanIdentityText(p.Text)})
					} else {
						systemParts = append(systemParts, map[string]interface{}{"type": p.Type, "text": p.Text})
					}
				}
				continue
			}

			antMsg := map[string]interface{}{
				"role":    role,
				"content": []interface{}{},
			}
			text, parts := parseContentParts(msg["content"])
			var contentParts []interface{}
			if text != "" {
				contentParts = append(contentParts, map[string]interface{}{"type": "text", "text": text})
			}
			for _, p := range parts {
				if p.Type == "text" {
					contentParts = append(contentParts, map[string]interface{}{"type": "text", "text": p.Text})
				} else if p.Type == "image_url" {
					contentParts = append(contentParts, map[string]interface{}{"type": "image", "source": p.ImageURL})
				} else {
					contentParts = append(contentParts, map[string]interface{}{"type": p.Type, "text": p.Text, "image_url": p.ImageURL})
				}
			}
			antMsg["content"] = contentParts
			messages = append(messages, antMsg)
		}
	}
	result["messages"] = messages

	// System prompt
	sysPrompt := c.SystemPrompt
	if sysPrompt != "" || len(systemParts) > 0 {
		var sys []interface{}
		if sysPrompt != "" {
			sys = append(sys, map[string]interface{}{"type": "text", "text": sysPrompt})
		}
		droidID := "You are Droid, an AI software engineering agent built by Factory."
		for _, sp := range systemParts {
			if txt, ok := sp["text"].(string); ok {
				cleaned := CleanIdentityText(txt)
				// Drop parts that became Droid-identity-only after replacement
				if strings.TrimSpace(cleaned) == droidID {
					continue
				}
				sp["text"] = cleaned
			}
			sys = append(sys, sp)
		}
		result["system"] = sys
	}

	// Tools
	if tools, ok := req["tools"].([]interface{}); ok {
		var antTools []interface{}
		for _, t := range tools {
			tool, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			if tool["type"] == "function" {
				fn, _ := tool["function"].(map[string]interface{})
				if fn != nil {
					antTools = append(antTools, map[string]interface{}{
						"name":         fn["name"],
						"description":  fn["description"],
						"input_schema": fn["parameters"],
					})
				}
			} else {
				antTools = append(antTools, tool)
			}
		}
		if len(antTools) > 0 {
			result["tools"] = antTools
		}
	}

	// Thinking/reasoning
	reasoningLevel := config.GetModelReasoning(modelID)
	switch reasoningLevel {
	case "auto":
		if thinking, ok := req["thinking"]; ok {
			result["thinking"] = thinking
		}
	case "low", "medium", "high", "xhigh":
		result["thinking"] = map[string]interface{}{
			"type":          "enabled",
			"budget_tokens": getReasoningBudget(reasoningLevel),
		}
	default:
		delete(result, "thinking")
	}

	// Pass through params
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := req[key]; ok {
			result[key] = v
		}
	}
	if stop, ok := req["stop"]; ok {
		switch s := stop.(type) {
		case []interface{}:
			result["stop_sequences"] = s
		case string:
			result["stop_sequences"] = []string{s}
		}
	}

	return result
}

func GetAnthropicHeaders(authHeader string, clientHeaders http.Header, isStreaming bool, modelID string, provider string) map[string]string {
	c := config.Get()
	sessionID := clientHeaders.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = generateUUID()
	}
	messageID := clientHeaders.Get("X-Assistant-Message-Id")
	if messageID == "" {
		messageID = generateUUID()
	}

	headers := map[string]string{
		"accept":                  "application/json",
		"content-type":           "application/json",
		"anthropic-version":      firstNonEmpty(clientHeaders.Get("Anthropic-Version"), "2023-06-01"),
		"authorization":          authHeader,
		"x-api-key":             "placeholder",
		"x-api-provider":        provider,
		"x-factory-client":      "cli",
		"x-session-id":          sessionID,
		"x-assistant-message-id": messageID,
		"user-agent":            c.UserAgent,
		"x-stainless-timeout":   "600",
		"connection":            "keep-alive",
	}

	// Handle anthropic-beta
	reasoningLevel := config.GetModelReasoning(modelID)
	var betaValues []string
	if cb := clientHeaders.Get("Anthropic-Beta"); cb != "" {
		betaValues = strings.Split(cb, ",")
		for i := range betaValues {
			betaValues[i] = strings.TrimSpace(betaValues[i])
		}
	}

	thinkingBeta := "interleaved-thinking-2025-05-14"
	switch reasoningLevel {
	case "auto":
		// keep original
	case "low", "medium", "high", "xhigh":
		if !containsStr(betaValues, thinkingBeta) {
			betaValues = append(betaValues, thinkingBeta)
		}
	default:
		betaValues = filterStr(betaValues, thinkingBeta)
	}

	fastModeBeta := "fast-mode-2026-02-01"
	if config.IsModelFast(modelID) {
		if !containsStr(betaValues, fastModeBeta) {
			betaValues = append(betaValues, fastModeBeta)
		}
	}

	if len(betaValues) > 0 {
		headers["anthropic-beta"] = strings.Join(betaValues, ", ")
	}

	if isStreaming {
		headers["x-stainless-helper-method"] = "stream"
	}

	// Stainless defaults
	stainless := map[string]string{
		"x-stainless-arch":            "x64",
		"x-stainless-lang":            "js",
		"x-stainless-os":              "MacOS",
		"x-stainless-runtime":         "node",
		"x-stainless-retry-count":     "0",
		"x-stainless-package-version": "0.57.0",
		"x-stainless-runtime-version": "v24.3.0",
	}
	for k, def := range stainless {
		if v := clientHeaders.Get(k); v != "" {
			headers[k] = v
		} else {
			headers[k] = def
		}
	}
	if v := clientHeaders.Get("X-Stainless-Timeout"); v != "" {
		headers["x-stainless-timeout"] = v
	}

	return headers
}

// ---- OpenAI request transformer ----

func TransformToOpenAI(req map[string]interface{}) map[string]interface{} {
	c := config.Get()
	modelID, _ := req["model"].(string)

	result := map[string]interface{}{
		"model": modelID,
		"input": []interface{}{},
		"store": false,
	}

	if stream, ok := req["stream"]; ok {
		result["stream"] = stream
	}
	if mt, ok := req["max_tokens"]; ok {
		result["max_output_tokens"] = mt
	} else if mct, ok := req["max_completion_tokens"]; ok {
		result["max_output_tokens"] = mct
	}

	var inputMessages []interface{}
	if msgs, ok := req["messages"].([]interface{}); ok {
		for _, raw := range msgs {
			msg, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			textType := "input_text"
			if role == "assistant" {
				textType = "output_text"
			}

			inputMsg := map[string]interface{}{
				"role":    role,
				"content": []interface{}{},
			}
			text, parts := parseContentParts(msg["content"])
			var contentParts []interface{}
			if text != "" {
				contentParts = append(contentParts, map[string]interface{}{"type": textType, "text": text})
			}
			for _, p := range parts {
				if p.Type == "text" {
					contentParts = append(contentParts, map[string]interface{}{"type": textType, "text": p.Text})
				} else if p.Type == "image_url" {
					imgType := textType
					if role == "assistant" {
						imgType = "output_image"
					} else {
						imgType = "input_image"
					}
					contentParts = append(contentParts, map[string]interface{}{"type": imgType, "image_url": p.ImageURL})
				} else {
					contentParts = append(contentParts, map[string]interface{}{"type": p.Type, "text": p.Text})
				}
			}
			inputMsg["content"] = contentParts
			inputMessages = append(inputMessages, inputMsg)
		}
	}

	// Extract system messages as instructions
	sysPrompt := c.SystemPrompt
	var systemMsgContent string
	filtered := inputMessages[:0]
	for _, raw := range inputMessages {
		msg, _ := raw.(map[string]interface{})
		role, _ := msg["role"].(string)
		if role == "system" {
			// Extract text
			if content, ok := msg["content"].([]interface{}); ok {
				for _, c := range content {
					cp, _ := c.(map[string]interface{})
					if t, _ := cp["type"].(string); t == "input_text" {
						if txt, ok := cp["text"].(string); ok {
							systemMsgContent += txt + "\n"
						}
					}
				}
			}
			continue
		}
		filtered = append(filtered, raw)
	}
	result["input"] = filtered

	if systemMsgContent != "" || sysPrompt != "" {
		result["instructions"] = sysPrompt + CleanIdentityText(systemMsgContent)
	}

	// Tools
	if tools, ok := req["tools"].([]interface{}); ok {
		var oaiTools []interface{}
		for _, t := range tools {
			tool, _ := t.(map[string]interface{})
			tc := copyMap(tool)
			tc["strict"] = false
			oaiTools = append(oaiTools, tc)
		}
		result["tools"] = oaiTools
	}

	// Reasoning
	reasoningLevel := config.GetModelReasoning(modelID)
	switch reasoningLevel {
	case "auto":
		if r, ok := req["reasoning"]; ok {
			result["reasoning"] = r
		}
	case "low", "medium", "high", "xhigh":
		result["reasoning"] = map[string]interface{}{
			"effort":  reasoningLevel,
			"summary": "auto",
		}
	default:
		delete(result, "reasoning")
	}

	// Fast model service_tier
	if config.IsModelFast(modelID) {
		if _, ok := req["service_tier"]; !ok {
			result["service_tier"] = "priority"
		}
	}
	if st, ok := req["service_tier"]; ok {
		result["service_tier"] = st
	}

	for _, key := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty", "parallel_tool_calls"} {
		if v, ok := req[key]; ok {
			result[key] = v
		}
	}

	return result
}

func GetOpenAIHeaders(authHeader string, clientHeaders http.Header, provider string) map[string]string {
	c := config.Get()
	sessionID := clientHeaders.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = generateUUID()
	}
	messageID := clientHeaders.Get("X-Assistant-Message-Id")
	if messageID == "" {
		messageID = generateUUID()
	}

	headers := map[string]string{
		"content-type":           "application/json",
		"authorization":          authHeader,
		"x-api-provider":        provider,
		"x-factory-client":      "cli",
		"x-session-id":          sessionID,
		"x-assistant-message-id": messageID,
		"user-agent":            c.UserAgent,
		"connection":            "keep-alive",
	}

	stainless := map[string]string{
		"x-stainless-arch":            "x64",
		"x-stainless-lang":            "js",
		"x-stainless-os":              "MacOS",
		"x-stainless-runtime":         "node",
		"x-stainless-retry-count":     "0",
		"x-stainless-package-version": "5.23.2",
		"x-stainless-runtime-version": "v24.3.0",
	}
	for k, def := range stainless {
		if v := clientHeaders.Get(k); v != "" {
			headers[k] = v
		} else {
			headers[k] = def
		}
	}
	return headers
}

// ---- Common request transformer ----

func TransformToCommon(req map[string]interface{}) map[string]interface{} {
	c := config.Get()
	modelID, _ := req["model"].(string)
	result := copyMap(req)

	sysPrompt := c.SystemPrompt
	if sysPrompt != "" {
		msgs, _ := result["messages"].([]interface{})
		hasSystem := false
		for _, raw := range msgs {
			if msg, ok := raw.(map[string]interface{}); ok {
				if role, _ := msg["role"].(string); role == "system" {
					hasSystem = true
					break
				}
			}
		}
		if hasSystem {
			for i, raw := range msgs {
				msg, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				if role, _ := msg["role"].(string); role == "system" {
					content, _ := msg["content"].(string)
					msg["content"] = sysPrompt + CleanIdentityText(content)
					msgs[i] = msg
					break
				}
			}
		} else {
			newMsgs := []interface{}{map[string]interface{}{"role": "system", "content": sysPrompt}}
			newMsgs = append(newMsgs, msgs...)
			result["messages"] = newMsgs
		}
	}

	reasoningLevel := config.GetModelReasoning(modelID)
	switch reasoningLevel {
	case "auto":
		// keep
	case "low", "medium", "high", "xhigh":
		result["reasoning_effort"] = reasoningLevel
	default:
		delete(result, "reasoning_effort")
	}

	return result
}

func GetCommonHeaders(authHeader string, clientHeaders http.Header, provider string) map[string]string {
	c := config.Get()
	sessionID := clientHeaders.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = generateUUID()
	}
	messageID := clientHeaders.Get("X-Assistant-Message-Id")
	if messageID == "" {
		messageID = generateUUID()
	}

	headers := map[string]string{
		"accept":                  "application/json",
		"content-type":           "application/json",
		"authorization":          authHeader,
		"x-api-provider":        provider,
		"x-factory-client":      "cli",
		"x-session-id":          sessionID,
		"x-assistant-message-id": messageID,
		"user-agent":            c.UserAgent,
		"connection":            "keep-alive",
	}

	stainless := map[string]string{
		"x-stainless-arch":            "x64",
		"x-stainless-lang":            "js",
		"x-stainless-os":              "MacOS",
		"x-stainless-runtime":         "node",
		"x-stainless-retry-count":     "0",
		"x-stainless-package-version": "5.23.2",
		"x-stainless-runtime-version": "v24.3.0",
	}
	for k, def := range stainless {
		if v := clientHeaders.Get(k); v != "" {
			headers[k] = v
		} else {
			headers[k] = def
		}
	}
	return headers
}

// ---- Google request transformer ----

func TransformToGoogle(req map[string]interface{}) map[string]interface{} {
	c := config.Get()
	modelID, _ := req["model"].(string)

	result := map[string]interface{}{
		"model":    modelID,
		"contents": []interface{}{},
	}

	var systemParts []interface{}
	sysPrompt := c.SystemPrompt
	if sysPrompt != "" {
		systemParts = append(systemParts, map[string]interface{}{"text": sysPrompt})
	}

	var contents []interface{}
	if msgs, ok := req["messages"].([]interface{}); ok {
		for _, raw := range msgs {
			msg, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)

			if role == "system" {
				text, parts := parseContentParts(msg["content"])
				if text != "" {
					systemParts = append(systemParts, map[string]interface{}{"text": CleanIdentityText(text)})
				}
				for _, p := range parts {
					if p.Type == "text" {
						systemParts = append(systemParts, map[string]interface{}{"text": CleanIdentityText(p.Text)})
					}
				}
				continue
			}

			googleRole := role
			if role == "assistant" {
				googleRole = "model"
			}
			gMsg := map[string]interface{}{
				"role":  googleRole,
				"parts": []interface{}{},
			}
			text, parts := parseContentParts(msg["content"])
			var gParts []interface{}
			if text != "" {
				gParts = append(gParts, map[string]interface{}{"text": text})
			}
			for _, p := range parts {
				if p.Type == "text" {
					gParts = append(gParts, map[string]interface{}{"text": p.Text})
				} else if p.Type == "image_url" {
					imgURL, _ := p.ImageURL.(map[string]interface{})
					gParts = append(gParts, map[string]interface{}{
						"inlineData": map[string]interface{}{
							"mimeType": firstNonEmpty(fmt.Sprintf("%v", imgURL["type"]), "image/jpeg"),
							"data":     imgURL["url"],
						},
					})
				} else {
					gParts = append(gParts, map[string]interface{}{"text": p.Text})
				}
			}
			gMsg["parts"] = gParts
			contents = append(contents, gMsg)
		}
	}
	result["contents"] = contents

	if len(systemParts) > 0 {
		result["systemInstruction"] = map[string]interface{}{"parts": systemParts}
	}

	// generationConfig
	genCfg := map[string]interface{}{}
	if mt, ok := req["max_tokens"]; ok {
		genCfg["maxOutputTokens"] = mt
	} else if mct, ok := req["max_completion_tokens"]; ok {
		genCfg["maxOutputTokens"] = mct
	}
	if v, ok := req["temperature"]; ok {
		genCfg["temperature"] = v
	}
	if v, ok := req["top_p"]; ok {
		genCfg["topP"] = v
	}
	if stop, ok := req["stop"]; ok {
		switch s := stop.(type) {
		case []interface{}:
			genCfg["stopSequences"] = s
		case string:
			genCfg["stopSequences"] = []string{s}
		}
	}
	if v, ok := req["presence_penalty"]; ok {
		genCfg["presencePenalty"] = v
	}
	if v, ok := req["frequency_penalty"]; ok {
		genCfg["frequencyPenalty"] = v
	}

	reasoningLevel := config.GetModelReasoning(modelID)
	switch reasoningLevel {
	case "auto":
		// keep original
	case "low", "medium", "high":
		levelMap := map[string]string{"low": "LOW", "medium": "MEDIUM", "high": "HIGH"}
		genCfg["thinkingConfig"] = map[string]interface{}{"thinkingLevel": levelMap[reasoningLevel]}
	default:
		delete(genCfg, "thinkingConfig")
	}

	if len(genCfg) > 0 {
		result["generationConfig"] = genCfg
	}

	// Tools
	if tools, ok := req["tools"].([]interface{}); ok {
		var funcDecls []interface{}
		for _, t := range tools {
			tool, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			if tool["type"] == "function" {
				fn, _ := tool["function"].(map[string]interface{})
				if fn != nil {
					funcDecls = append(funcDecls, map[string]interface{}{
						"name":        fn["name"],
						"description": fn["description"],
						"parameters":  fn["parameters"],
					})
				}
			}
		}
		if len(funcDecls) > 0 {
			result["tools"] = []interface{}{map[string]interface{}{"functionDeclarations": funcDecls}}
		}
	}

	return result
}

func GetGoogleHeaders(authHeader string, clientHeaders http.Header, provider string) map[string]string {
	c := config.Get()
	sessionID := clientHeaders.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = generateUUID()
	}
	messageID := clientHeaders.Get("X-Assistant-Message-Id")
	if messageID == "" {
		messageID = generateUUID()
	}

	ua := c.UserAgent
	clientVersion := "0.84.0"
	if idx := strings.Index(ua, "/"); idx >= 0 && idx+1 < len(ua) {
		clientVersion = ua[idx+1:]
	}

	headers := map[string]string{
		"accept":                  "*/*",
		"content-type":           "application/json",
		"authorization":          authHeader,
		"user-agent":            ua,
		"x-client-version":      clientVersion,
		"x-factory-client":      firstNonEmpty(clientHeaders.Get("X-Factory-Client"), "cli"),
		"x-api-provider":        provider,
		"x-assistant-message-id": messageID,
		"x-session-id":          sessionID,
		"connection":            "keep-alive",
	}
	return headers
}

// ---- Utility helpers ----

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func containsStr(arr []string, s string) bool {
	for _, v := range arr {
		if v == s {
			return true
		}
	}
	return false
}

func filterStr(arr []string, exclude string) []string {
	var result []string
	for _, v := range arr {
		if v != exclude {
			result = append(result, v)
		}
	}
	return result
}

func copyMap(src map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
