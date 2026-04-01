package transformer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ---- OpenAI chunk format (output) ----

type chatChunkChoice struct {
	Index        int                    `json:"index"`
	Delta        map[string]interface{} `json:"delta"`
	FinishReason interface{}            `json:"finish_reason"` // string or null
}

type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   map[string]int    `json:"usage,omitempty"`
}

func newChunk(id, model string, created int64, content string, role string, finish bool, finishReason string) string {
	c := chatChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []chatChunkChoice{
			{
				Index: 0,
				Delta: map[string]interface{}{},
			},
		},
	}
	if role != "" {
		c.Choices[0].Delta["role"] = role
	}
	if content != "" {
		c.Choices[0].Delta["content"] = content
	}
	if finish {
		c.Choices[0].FinishReason = finishReason
	}

	b, _ := json.Marshal(c)
	return "data: " + string(b) + "\n\n"
}

func doneSignal() string {
	return "data: [DONE]\n\n"
}

// ---- Anthropic response stream transformer ----

// TransformAnthropicStream reads an Anthropic SSE stream and yields OpenAI-compatible SSE chunks.
func TransformAnthropicStream(body io.ReadCloser, model string, w io.Writer, flush func()) error {
	defer body.Close()
	requestID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024) // 1MB buffer
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(line[6:])
			continue
		}

		if strings.HasPrefix(line, "data:") && currentEvent != "" {
			dataStr := strings.TrimSpace(line[5:])
			var eventData map[string]interface{}
			if err := json.Unmarshal([]byte(dataStr), &eventData); err != nil {
				currentEvent = ""
				continue
			}

			var chunk string
			switch currentEvent {
			case "message_start":
				chunk = newChunk(requestID, model, created, "", "assistant", false, "")
			case "content_block_delta":
				delta, _ := eventData["delta"].(map[string]interface{})
				text, _ := delta["text"].(string)
				if text != "" {
					chunk = newChunk(requestID, model, created, text, "", false, "")
				}
			case "message_delta":
				delta, _ := eventData["delta"].(map[string]interface{})
				stopReason, _ := delta["stop_reason"].(string)
				if stopReason != "" {
					fr := mapAnthropicStopReason(stopReason)
					chunk = newChunk(requestID, model, created, "", "", true, fr)
				}
			case "message_stop":
				chunk = doneSignal()
			}

			if chunk != "" {
				io.WriteString(w, chunk)
				flush()
			}
			currentEvent = ""
		}
	}

	return scanner.Err()
}

func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// ---- OpenAI response stream transformer ----

// TransformOpenAIStream reads a /v1/responses SSE stream and yields /v1/chat/completions-compatible chunks.
func TransformOpenAIStream(body io.ReadCloser, model string, w io.Writer, flush func()) error {
	defer body.Close()
	requestID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(line[6:])
			continue
		}

		if strings.HasPrefix(line, "data:") && currentEvent != "" {
			dataStr := strings.TrimSpace(line[5:])
			var eventData map[string]interface{}
			if err := json.Unmarshal([]byte(dataStr), &eventData); err != nil {
				currentEvent = ""
				continue
			}

			var chunk string
			switch currentEvent {
			case "response.created":
				chunk = newChunk(requestID, model, created, "", "assistant", false, "")
			case "response.output_text.delta":
				text := ""
				if d, ok := eventData["delta"].(string); ok {
					text = d
				} else if t, ok := eventData["text"].(string); ok {
					text = t
				}
				if text != "" {
					chunk = newChunk(requestID, model, created, text, "", false, "")
				}
			case "response.done":
				resp, _ := eventData["response"].(map[string]interface{})
				status, _ := resp["status"].(string)
				fr := "stop"
				if status == "incomplete" {
					fr = "length"
				}
				chunk = newChunk(requestID, model, created, "", "", true, fr) + doneSignal()
			}

			if chunk != "" {
				io.WriteString(w, chunk)
				flush()
			}
			currentEvent = ""
		}
	}

	return scanner.Err()
}

// ---- Google response stream transformer ----

// TransformGoogleStream reads a Google SSE stream and yields OpenAI-compatible SSE chunks.
func TransformGoogleStream(body io.ReadCloser, model string, w io.Writer, flush func()) error {
	defer body.Close()
	requestID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	created := time.Now().Unix()
	sentRole := false

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		dataStr := strings.TrimSpace(line[5:])
		var eventData map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &eventData); err != nil {
			continue
		}

		candidates, _ := eventData["candidates"].([]interface{})
		if len(candidates) == 0 {
			continue
		}
		candidate, _ := candidates[0].(map[string]interface{})
		if candidate == nil {
			continue
		}

		content, _ := candidate["content"].(map[string]interface{})
		parts, _ := content["parts"].([]interface{})
		finishReason, _ := candidate["finishReason"].(string)

		var result string

		for _, raw := range parts {
			part, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			// Skip thought parts
			if thought, ok := part["thought"].(bool); ok && thought {
				continue
			}
			text, _ := part["text"].(string)
			if text == "" && finishReason == "" {
				continue
			}
			if !sentRole {
				sentRole = true
				result += newChunk(requestID, model, created, "", "assistant", false, "")
			}
			if text != "" {
				result += newChunk(requestID, model, created, text, "", false, "")
			}
		}

		if finishReason != "" {
			fr := mapGoogleFinishReason(finishReason)

			// Build final chunk with optional usage
			c := chatChunk{
				ID:      requestID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []chatChunkChoice{
					{
						Index:        0,
						Delta:        map[string]interface{}{},
						FinishReason: fr,
					},
				},
			}

			// Inject usage if available
			if um, ok := eventData["usageMetadata"].(map[string]interface{}); ok {
				promptTokens := toInt(um["promptTokenCount"])
				completionTokens := toInt(um["candidatesTokenCount"])
				totalTokens := toInt(um["totalTokenCount"])
				c.Usage = map[string]int{
					"prompt_tokens":     promptTokens,
					"completion_tokens": completionTokens,
					"total_tokens":      totalTokens,
				}
			}

			b, _ := json.Marshal(c)
			result += "data: " + string(b) + "\n\n"
			result += doneSignal()
		}

		if result != "" {
			io.WriteString(w, result)
			flush()
		}
	}

	return scanner.Err()
}

func mapGoogleFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION":
		return "content_filter"
	default:
		return "stop"
	}
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// ---- Non-streaming response converters ----

// ConvertResponseToChatCompletion converts /v1/responses result to /v1/chat/completions format.
func ConvertResponseToChatCompletion(resp map[string]interface{}) map[string]interface{} {
	id, _ := resp["id"].(string)
	if strings.HasPrefix(id, "resp_") {
		id = "chatcmpl-" + id[5:]
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli())
	}

	model, _ := resp["model"].(string)

	// Find message output
	var content string
	var role string
	if output, ok := resp["output"].([]interface{}); ok {
		for _, raw := range output {
			o, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if o["type"] == "message" {
				role, _ = o["role"].(string)
				if parts, ok := o["content"].([]interface{}); ok {
					for _, p := range parts {
						part, _ := p.(map[string]interface{})
						if part["type"] == "output_text" {
							if txt, ok := part["text"].(string); ok {
								content += txt
							}
						}
					}
				}
				break
			}
		}
	}
	if role == "" {
		role = "assistant"
	}

	status, _ := resp["status"].(string)
	finishReason := "stop"
	if status != "completed" {
		finishReason = "unknown"
	}

	usage := map[string]int{
		"prompt_tokens":     0,
		"completion_tokens": 0,
		"total_tokens":      0,
	}
	if u, ok := resp["usage"].(map[string]interface{}); ok {
		usage["prompt_tokens"] = toInt(u["input_tokens"])
		usage["completion_tokens"] = toInt(u["output_tokens"])
		usage["total_tokens"] = toInt(u["total_tokens"])
	}

	return map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role":    role,
					"content": content,
				},
				"finish_reason": finishReason,
			},
		},
		"usage": usage,
	}
}

// ConvertGoogleResponseToChatCompletion converts Google generateContent result to chat completion format.
func ConvertGoogleResponseToChatCompletion(resp map[string]interface{}, modelID string) map[string]interface{} {
	var content string
	finishReason := "stop"

	if candidates, ok := resp["candidates"].([]interface{}); ok && len(candidates) > 0 {
		candidate, _ := candidates[0].(map[string]interface{})
		if c, ok := candidate["content"].(map[string]interface{}); ok {
			if parts, ok := c["parts"].([]interface{}); ok {
				for _, raw := range parts {
					part, _ := raw.(map[string]interface{})
					if thought, ok := part["thought"].(bool); ok && thought {
						continue
					}
					if txt, ok := part["text"].(string); ok {
						content += txt
					}
				}
			}
		}
		if fr, ok := candidate["finishReason"].(string); ok {
			finishReason = mapGoogleFinishReason(fr)
		}
	}

	usage := map[string]int{
		"prompt_tokens":     0,
		"completion_tokens": 0,
		"total_tokens":      0,
	}
	if um, ok := resp["usageMetadata"].(map[string]interface{}); ok {
		usage["prompt_tokens"] = toInt(um["promptTokenCount"])
		usage["completion_tokens"] = toInt(um["candidatesTokenCount"])
		usage["total_tokens"] = toInt(um["totalTokenCount"])
	}

	return map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelID,
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": finishReason,
			},
		},
		"usage": usage,
	}
}
