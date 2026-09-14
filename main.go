package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

const (
	defaultListenAddr = "127.0.0.1:20129"
	defaultDataPath   = "/root/agrouter-data/accounts.json"
	adminToken        = "agrouter-admin" // override via AGR_ADMIN_TOKEN
)

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getGSuiteDir() string {
	if dir := os.Getenv("AG_GSUITE_DIR"); dir != "" {
		return dir
	}
	candidates := []string{
		"ag_gsuite",
		"./ag_gsuite",
		"/root/ag_gsuite",
	}
	for _, c := range candidates {
		if fi, err := os.Stat(filepath.Join(c, "ag_gsuite.js")); err == nil && !fi.IsDir() {
			return c
		}
	}
	return "ag_gsuite"
}

var store *Store

// =====================================================================
// OpenAI request → antigravity (Gemini-style v1internal) translation
// =====================================================================

type oaiMessage struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

type oaiRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Stream      bool         `json:"stream"`
	MaxTokens   *int         `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	TopP        *float64     `json:"top_p,omitempty"`
	Tools       []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools,omitempty"`
	ToolChoice interface{} `json:"tool_choice,omitempty"`
}

// agPart is a Gemini-style content part.
type agFuncCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	ID   string          `json:"id,omitempty"`
}

type agFuncResp struct {
	Name     string      `json:"name"`
	Response interface{} `json:"response"`
	ID       string      `json:"id,omitempty"`
}

type agPart struct {
	Text             string      `json:"text,omitempty"`
	ThoughtSignature string      `json:"thoughtSignature,omitempty"`
	FunctionCall     *agFuncCall `json:"functionCall,omitempty"`
	FunctionResponse *agFuncResp `json:"functionResponse,omitempty"`
}

const sentinelThoughtSignature = "skip_thought_signature_validator"

var (
	sigCacheMu sync.Mutex
	sigCache   = make(map[string]string)

	toolNameCacheMu sync.Mutex
	toolNameCache   = make(map[string]string)
)

func cacheSig(callID, sig string) {
	if callID == "" || sig == "" {
		return
	}
	sigCacheMu.Lock()
	if len(sigCache) > 4000 {
		sigCache = make(map[string]string)
	}
	sigCache[callID] = sig
	sigCacheMu.Unlock()
}

func getSig(callID string) string {
	if callID != "" {
		sigCacheMu.Lock()
		sig, ok := sigCache[callID]
		sigCacheMu.Unlock()
		if ok && sig != "" {
			return sig
		}
	}
	return sentinelThoughtSignature
}

func cacheToolName(callID, name string) {
	if callID == "" || name == "" {
		return
	}
	toolNameCacheMu.Lock()
	if len(toolNameCache) > 4000 {
		toolNameCache = make(map[string]string)
	}
	toolNameCache[callID] = name
	toolNameCacheMu.Unlock()
}

func getToolName(callID string) string {
	if callID == "" {
		return ""
	}
	toolNameCacheMu.Lock()
	n := toolNameCache[callID]
	toolNameCacheMu.Unlock()
	return n
}

type agContent struct {
	Role  string   `json:"role"`
	Parts []agPart `json:"parts"`
}

type agFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type agTool struct {
	FunctionDeclarations []agFuncDecl `json:"functionDeclarations"`
}

// agPayload is the WIRE format for cloudcode-pa v1internal: top-level
// project + model, with the Gemini-style body nested under "request"
// (9router chunks/318.js transformRequest).
type agPayload struct {
	Project string `json:"project,omitempty"`
	Model   string `json:"model"`
	Request struct {
		Contents          []agContent `json:"contents"`
		SystemInstruction *struct {
			Parts []agPart `json:"parts"`
		} `json:"systemInstruction,omitempty"`
		Tools            []agTool `json:"tools,omitempty"`
		GenerationConfig struct {
			MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
			Temperature     *float64 `json:"temperature,omitempty"`
			TopP            *float64 `json:"topP,omitempty"`
			ThinkingConfig  *struct {
				ThinkingBudget int `json:"thinkingBudget"`
			} `json:"thinkingConfig,omitempty"`
		} `json:"generationConfig"`
	} `json:"request"`
}

func contentToText(c interface{}) string {
	switch v := c.(type) {
	case string:
		return v
	case []interface{}:
		var b strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	default:
		if v != nil {
			b, _ := json.Marshal(v)
			return string(b)
		}
	}
	return ""
}

var modelSynonym = map[string]string{
	"deepseek-v4-pro-max":        "deepseek-v4-pro",
	"deepseek-v4-pro-none":       "deepseek-v4-pro",
	"claude-3-5-sonnet":          "claude-sonnet-4-6",
	"claude-3-7-sonnet":          "claude-sonnet-4-6",
	"claude-3-5-sonnet-thinking": "claude-opus-4-6-thinking",
	"claude-3-7-sonnet-thinking": "claude-opus-4-6-thinking",
}

// tieredRe: gemini-3.6/3.7/3.8 flash levels ride on the bare "-tiered" upstream id,
// with thinking strength as generationConfig.thinkingConfig.thinkingBudget
// (9router 8499.js format "gemini-budget" + nE mapping).
var tieredRe = regexp.MustCompile(`^gemini-(3\.[678])-flash-(high|medium|low)$`)

var levelBudget = map[string]int{"low": 1024, "medium": 8192, "high": 24576}

// resolveUpstreamModel splits a public id into (wireModel, thinkingBudget).
// budget nil = don't send thinkingConfig at all.
func resolveUpstreamModel(m string) (string, *int) {
	m = strings.TrimPrefix(m, "ag/")
	if s, ok := modelSynonym[m]; ok {
		return s, nil
	}
	if m == "gemini-3.8-flash" || m == "gemini-3.7-flash" || m == "gemini-3.6-flash" {
		b := 24576
		return m + "-tiered", &b
	}
	if mm := tieredRe.FindStringSubmatch(m); mm != nil {
		b := levelBudget[mm[2]]
		return fmt.Sprintf("gemini-%s-flash-tiered", mm[1]), &b
	}
	return m, nil
}

func sanitizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	v = cleanSchemaNode(v)
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

func cleanSchemaNode(node interface{}) interface{} {
	switch v := node.(type) {
	case []interface{}:
		for i, it := range v {
			v[i] = cleanSchemaNode(it)
		}
		return v
	case map[string]interface{}:
		// 1. Google Gemini Schema enum MUST be strings (protobuf: repeated string enum)
		// Numbers/integers in enum (e.g. discord auto_archive_duration: [60, 1440...])
		// trigger 400 INVALID_ARGUMENT (TYPE_STRING violation).
		if enums, exists := v["enum"]; exists {
			if enumList, ok := enums.([]interface{}); ok {
				var strEnums []string
				for _, e := range enumList {
					strEnums = append(strEnums, fmt.Sprintf("%v", e))
				}
				v["enum"] = strEnums
				if t, ok := v["type"].(string); ok && (t == "integer" || t == "number") {
					v["type"] = "string"
				}
			}
		}
		// 2. Type array like ["string", "null"] -> "string"
		if t, exists := v["type"]; exists {
			if tList, ok := t.([]interface{}); ok && len(tList) > 0 {
				for _, elem := range tList {
					if s, ok := elem.(string); ok && s != "null" {
						v["type"] = s
						break
					}
				}
			}
		}
		// 3. Recurse properties
		if props, exists := v["properties"]; exists {
			if propMap, ok := props.(map[string]interface{}); ok {
				for k, p := range propMap {
					propMap[k] = cleanSchemaNode(p)
				}
			}
		}
		// 4. Recurse items
		if items, exists := v["items"]; exists {
			v["items"] = cleanSchemaNode(items)
		}
		return v
	default:
		return node
	}
}

func buildAGRequest(o *oaiRequest, upstreamModel string, thinkingBudget *int, projectID string) []byte {
	ag := &agPayload{Project: projectID, Model: upstreamModel}
	var sys strings.Builder

	callIDToName := make(map[string]string)
	for _, msg := range o.Messages {
		if msg.Role == "assistant" {
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" && tc.Function.Name != "" {
					callIDToName[tc.ID] = tc.Function.Name
					cacheToolName(tc.ID, tc.Function.Name)
				}
			}
		}
	}

	for _, msg := range o.Messages {
		text := contentToText(msg.Content)
		switch msg.Role {
		case "system", "developer":
			if sys.Len() > 0 {
				sys.WriteString("\n\n")
			}
			sys.WriteString(text)
		case "user":
			ag.Request.Contents = append(ag.Request.Contents, agContent{Role: "user", Parts: []agPart{{Text: text}}})
		case "assistant":
			var parts []agPart
			if text != "" {
				parts = append(parts, agPart{Text: text})
			}
			for _, tc := range msg.ToolCalls {
				rawArgs := json.RawMessage(tc.Function.Arguments)
				if len(rawArgs) == 0 {
					rawArgs = json.RawMessage("{}")
				}
				callID := tc.ID
				if callID == "" {
					callID = "call_" + newID()
				}
				if tc.Function.Name != "" {
					callIDToName[callID] = tc.Function.Name
					cacheToolName(callID, tc.Function.Name)
				}
				sig := getSig(callID)
				parts = append(parts, agPart{
					ThoughtSignature: sig,
					FunctionCall: &agFuncCall{
						Name: tc.Function.Name,
						Args: rawArgs,
						ID:   callID,
					},
				})
			}
			if len(parts) > 0 {
				ag.Request.Contents = append(ag.Request.Contents, agContent{Role: "model", Parts: parts})
			}
		case "tool":
			toolName := msg.Name
			if toolName == "" {
				if n, ok := callIDToName[msg.ToolCallID]; ok && n != "" {
					toolName = n
				} else if n := getToolName(msg.ToolCallID); n != "" {
					toolName = n
				}
			}
			if toolName == "" {
				toolName = "tool"
			}
			var parsedResp interface{}
			if err := json.Unmarshal([]byte(text), &parsedResp); err != nil {
				parsedResp = map[string]interface{}{"output": text}
			} else if _, ok := parsedResp.(map[string]interface{}); !ok {
				parsedResp = map[string]interface{}{"result": parsedResp}
			}
			ag.Request.Contents = append(ag.Request.Contents, agContent{
				Role: "user",
				Parts: []agPart{
					{
						FunctionResponse: &agFuncResp{
							Name: toolName,
							ID:   msg.ToolCallID,
							Response: map[string]interface{}{
								"result": parsedResp,
							},
						},
					},
				},
			})
		}
	}
	if sys.Len() > 0 {
		ag.Request.SystemInstruction = &struct {
			Parts []agPart `json:"parts"`
		}{Parts: []agPart{{Text: sys.String()}}}
	}
	for _, t := range o.Tools {
		if t.Type == "function" {
			ag.Request.Tools = append(ag.Request.Tools, agTool{FunctionDeclarations: []agFuncDecl{{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  sanitizeSchema(t.Function.Parameters),
			}}})
		}
	}
	if o.MaxTokens != nil {
		mt := *o.MaxTokens
		// Google cap: maxOutputTokens > 32768 → 400 INVALID_ARGUMENT on the SSE path
		// (verified: 65536 always 400, 32768 always 200). Clamp, don't forward.
		if mt > 32768 {
			mt = 32768
		}
		ag.Request.GenerationConfig.MaxOutputTokens = &mt
	} else {
		// Reasoning models burn hidden thinking tokens from the same budget —
		// a small/unset max_tokens yields empty content (pitfall: max_tokens starvation).
		def := 32768
		ag.Request.GenerationConfig.MaxOutputTokens = &def
	}
	if thinkingBudget != nil {
		ag.Request.GenerationConfig.ThinkingConfig = &struct {
			ThinkingBudget int `json:"thinkingBudget"`
		}{ThinkingBudget: *thinkingBudget}
	}
	ag.Request.GenerationConfig.Temperature = o.Temperature
	ag.Request.GenerationConfig.TopP = o.TopP
	// PR 3366 parity (decolua/9router): drop EVERY content whose parts end up empty —
	// thinking-only assistant turns, blank tool results, blank user turns — otherwise
	// Google rejects the whole request with 400 INVALID_ARGUMENT.
	filtered := ag.Request.Contents[:0]
	for _, c := range ag.Request.Contents {
		hasPart := false
		for _, p := range c.Parts {
			if strings.TrimSpace(p.Text) != "" || p.FunctionCall != nil || p.FunctionResponse != nil {
				hasPart = true
				break
			}
		}
		if hasPart {
			filtered = append(filtered, c)
		}
	}
	ag.Request.Contents = filtered

	b, _ := json.Marshal(ag)
	return b
}

// =====================================================================
// SSE translation: antigravity SSE → OpenAI chunk stream
// =====================================================================

type agUsageMetadata struct {
	PromptTokenCount             int `json:"promptTokenCount"`
	PromptTokenCountSnake        int `json:"prompt_token_count"`
	CandidatesTokenCount         int `json:"candidatesTokenCount"`
	CandidatesTokenCountSnake    int `json:"candidates_token_count"`
	TotalTokenCount              int `json:"totalTokenCount"`
	TotalTokenCountSnake         int `json:"total_token_count"`
	CachedContentTokenCount      int `json:"cachedContentTokenCount"`
	CachedContentTokenCountSnake int `json:"cached_content_token_count"`
	ThoughtsTokenCount           int `json:"thoughtsTokenCount"`
}

func (u *agUsageMetadata) Prompt() int {
	if u.PromptTokenCount > 0 {
		return u.PromptTokenCount
	}
	return u.PromptTokenCountSnake
}
func (u *agUsageMetadata) Output() int {
	if u.CandidatesTokenCount > 0 {
		return u.CandidatesTokenCount
	}
	return u.CandidatesTokenCountSnake
}
func (u *agUsageMetadata) Cached() int {
	if u.CachedContentTokenCount > 0 {
		return u.CachedContentTokenCount
	}
	return u.CachedContentTokenCountSnake
}
func (u *agUsageMetadata) Total() int {
	if u.TotalTokenCount > 0 {
		return u.TotalTokenCount
	}
	if u.TotalTokenCountSnake > 0 {
		return u.TotalTokenCountSnake
	}
	return u.Prompt() + u.Output()
}

func estimatePromptTokens(o *oaiRequest) int {
	chars := 0
	for _, m := range o.Messages {
		chars += len(contentToText(m.Content))
		for _, tc := range m.ToolCalls {
			chars += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	for _, t := range o.Tools {
		chars += len(t.Function.Name) + len(t.Function.Description) + len(t.Function.Parameters)
	}
	tok := chars / 4
	if tok < 1 {
		tok = 1
	}
	return tok
}

type agStreamChunk struct {
	Response struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text             string      `json:"text"`
					ThoughtSignature string      `json:"thoughtSignature,omitempty"`
					FunctionCall     *agFuncCall `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason  string `json:"finishReason,omitempty"`
			FinishMessage string `json:"finishMessage,omitempty"`
		} `json:"candidates"`
		UsageMetadata *agUsageMetadata `json:"usageMetadata,omitempty"`
	} `json:"response"`
	UsageMetadata *agUsageMetadata `json:"usageMetadata,omitempty"`
}

func sseTranslate(upstream io.Reader, model string, w io.Writer, flusher http.Flusher, approxPrompt int) (*TokenUsage, error) {
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	sendChunk := func(delta map[string]interface{}, finish *string) error {
		chunk := map[string]interface{}{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		b, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	sendChunk(map[string]interface{}{"role": "assistant"}, nil)

	hasToolCalls := false
	toolCallIndex := 0
	var lastUsage *agUsageMetadata
	outChars := 0

	handleObj := func(ck *agStreamChunk) (string, error) {
		if ck.Response.UsageMetadata != nil {
			lastUsage = ck.Response.UsageMetadata
		} else if ck.UsageMetadata != nil {
			lastUsage = ck.UsageMetadata
		}

		for _, cand := range ck.Response.Candidates {
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					outChars += len(p.Text)
					if err := sendChunk(map[string]interface{}{"content": p.Text}, nil); err != nil {
						return "", err
					}
				}
				if p.FunctionCall != nil {
					hasToolCalls = true
					callID := p.FunctionCall.ID
					if callID == "" {
						callID = fmt.Sprintf("call_%s_%d", p.FunctionCall.Name, toolCallIndex)
					}
					cacheToolName(callID, p.FunctionCall.Name)
					sig := p.ThoughtSignature
					if sig != "" {
						cacheSig(callID, sig)
					}
					argsStr := string(p.FunctionCall.Args)
					if argsStr == "" {
						argsStr = "{}"
					}
					tcDelta := map[string]interface{}{
						"index": toolCallIndex,
						"id":    callID,
						"type":  "function",
						"function": map[string]interface{}{
							"name":      p.FunctionCall.Name,
							"arguments": argsStr,
						},
					}
					toolCallIndex++
					if err := sendChunk(map[string]interface{}{
						"tool_calls": []interface{}{tcDelta},
					}, nil); err != nil {
						return "", err
					}
				}
			}
			if cand.FinishReason != "" {
				return cand.FinishReason, nil
			}
		}
		return "", nil
	}

	// Upstream with ?alt=sse sends SSE lines ("data: {...}"); without it, a
	// bare stream of JSON objects. Try SSE first, fall back to raw decoding.
	br := bufio.NewReaderSize(upstream, 1<<20)
	first, _ := br.Peek(6)
	isSSE := strings.HasPrefix(string(first), "data:")
	lastFinish := ""
	if isSSE {
		sc := bufio.NewScanner(br)
		sc.Buffer(make([]byte, 1024*1024), 8<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var ck agStreamChunk
			if err := json.Unmarshal([]byte(payload), &ck); err != nil {
				continue
			}
			fr, err := handleObj(&ck)
			if fr != "" {
				lastFinish = fr
			}
			if err != nil {
				return nil, err
			}
		}
	} else {
		dec := json.NewDecoder(br)
		for {
			var ck agStreamChunk
			if err := dec.Decode(&ck); err != nil {
				break
			}
			fr, err := handleObj(&ck)
			if fr != "" {
				lastFinish = fr
			}
			if err != nil {
				return nil, err
			}
		}
	}
	fr := "stop"
	if hasToolCalls {
		fr = "tool_calls"
	}
	if lastFinish == "MAX_TOKENS" {
		fr = "length"
	}

	tu := &TokenUsage{}
	if lastUsage != nil && (lastUsage.Prompt() > 0 || lastUsage.Output() > 0) {
		tu.Input = lastUsage.Prompt()
		tu.Output = lastUsage.Output()
		tu.Cached = lastUsage.Cached()
		tu.Total = lastUsage.Total()
	} else {
		tu.Input = approxPrompt
		tu.Output = outChars / 4
		if tu.Output < 1 {
			tu.Output = 1
		}
		tu.Total = tu.Input + tu.Output
	}

	finalUsageMap := map[string]interface{}{
		"prompt_tokens":     tu.Input,
		"completion_tokens": tu.Output,
		"total_tokens":      tu.Total,
	}
	if tu.Cached > 0 {
		finalUsageMap["prompt_tokens_details"] = map[string]interface{}{
			"cached_tokens": tu.Cached,
		}
	}

	finalChunk := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": &fr,
		}},
		"usage": finalUsageMap,
	}
	fb, _ := json.Marshal(finalChunk)
	if _, err := fmt.Fprintf(w, "data: %s\n\n", fb); err != nil {
		return tu, err
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
	return tu, nil
}

// nonStreamResponse builds a full OpenAI completion from an antigravity JSON response.
func nonStreamResponse(body []byte, model string, approxPrompt int) (*TokenUsage, []byte) {
	var ag agStreamChunk
	json.Unmarshal(body, &ag)
	var sb strings.Builder
	finish := "stop"
	var oaiToolCalls []map[string]interface{}
	tcIdx := 0

	for _, cand := range ag.Response.Candidates {
		for _, p := range cand.Content.Parts {
			if p.Text != "" {
				sb.WriteString(p.Text)
			}
			if p.FunctionCall != nil {
				callID := p.FunctionCall.ID
				if callID == "" {
					callID = fmt.Sprintf("call_%s_%d", p.FunctionCall.Name, tcIdx)
				}
				tcIdx++
				cacheToolName(callID, p.FunctionCall.Name)
				sig := p.ThoughtSignature
				if sig != "" {
					cacheSig(callID, sig)
				}
				argsStr := string(p.FunctionCall.Args)
				if argsStr == "" {
					argsStr = "{}"
				}
				oaiToolCalls = append(oaiToolCalls, map[string]interface{}{
					"id":   callID,
					"type": "function",
					"function": map[string]interface{}{
						"name":      p.FunctionCall.Name,
						"arguments": argsStr,
					},
				})
			}
		}
		if cand.FinishReason == "MAX_TOKENS" {
			finish = "length"
		}
	}

	if len(oaiToolCalls) > 0 && finish != "length" {
		finish = "tool_calls"
	}

	msg := map[string]interface{}{
		"role": "assistant",
	}
	if sb.Len() > 0 {
		msg["content"] = sb.String()
	} else if len(oaiToolCalls) > 0 {
		msg["content"] = nil
	} else {
		msg["content"] = ""
	}

	if len(oaiToolCalls) > 0 {
		msg["tool_calls"] = oaiToolCalls
	}

	tu := &TokenUsage{}
	var um *agUsageMetadata
	if ag.Response.UsageMetadata != nil {
		um = ag.Response.UsageMetadata
	} else if ag.UsageMetadata != nil {
		um = ag.UsageMetadata
	}
	if um != nil && (um.Prompt() > 0 || um.Output() > 0) {
		tu.Input = um.Prompt()
		tu.Output = um.Output()
		tu.Cached = um.Cached()
		tu.Total = um.Total()
	} else {
		tu.Input = approxPrompt
		tu.Output = sb.Len() / 4
		if tu.Output < 1 {
			tu.Output = 1
		}
		tu.Total = tu.Input + tu.Output
	}

	respUsage := map[string]interface{}{
		"prompt_tokens":     tu.Input,
		"completion_tokens": tu.Output,
		"total_tokens":      tu.Total,
	}
	if tu.Cached > 0 {
		respUsage["prompt_tokens_details"] = map[string]interface{}{
			"cached_tokens": tu.Cached,
		}
	}

	resp := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": respUsage,
	}
	b, _ := json.Marshal(resp)
	return tu, b
}

// =====================================================================
// Proxy endpoint: POST /v1/chat/completions
// =====================================================================

var fluxMu sync.Mutex // serialize token refresh bursts on first boot

// ---------- log ring buffer (console) ----------
const logBufSize = 500

var (
	logMu       sync.Mutex
	logBuf      []string
	logSeq      uint64
	logWatchers map[chan string]struct{}
)

func init() { logWatchers = map[chan string]struct{}{} }

// pushLog stores a line and broadcasts to SSE watchers.
func pushLog(line string) {
	logMu.Lock()
	if len(logBuf) >= logBufSize {
		logBuf = logBuf[1:]
	}
	logBuf = append(logBuf, line)
	logSeq++
	watchers := make([]chan string, 0, len(logWatchers))
	for ch := range logWatchers {
		watchers = append(watchers, ch)
	}
	logMu.Unlock()
	for _, ch := range watchers {
		select { case ch <- line: default: } // drop if slow
	}
}

// slogf writes to both stderr log and the ring buffer (with HH:MM:SS timestamp
// embedded so the dashboard console shows when each event happened).
func slogf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	pushLog(time.Now().UTC().Format("15:04:05") + " " + msg)
}

// handleLogs: GET /admin/logs → ring buffer snapshot; GET /admin/logs?stream=1 → SSE tail.
// Note: EventSource API cannot set headers, so the stream accepts ?token= as well.
func handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("stream") != "1" {
		logMu.Lock()
		lines := append([]string(nil), logBuf...)
		logMu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"lines": lines})
		return
	}
	// manual auth for the SSE path (header OR ?token=)
	tok := ""
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		tok = strings.TrimPrefix(ah, "Bearer ")
	}
	if t := r.URL.Query().Get("token"); t != "" {
		tok = t
	}
	want := adminToken
	store.mu.Lock()
	if store.AdminToken != "" {
		want = store.AdminToken
	}
	store.mu.Unlock()
	if tok != want {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 64)
	logMu.Lock()
	logWatchers[ch] = struct{}{}
	// replay snapshot
	for _, l := range logBuf {
		fmt.Fprintf(w, "data: %s\n\n", l)
	}
	logMu.Unlock()
	flusher.Flush()
	defer func() {
		logMu.Lock()
		delete(logWatchers, ch)
		logMu.Unlock()
	}()
	notify := w.(http.CloseNotifier).CloseNotify()
	for {
		select {
		case <-notify:
			return
		case line := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}
}

// handleClearLogs wipes the ring buffer.
func handleClearLogs(w http.ResponseWriter, r *http.Request) {
	logMu.Lock()
	logBuf = nil
	logMu.Unlock()
	w.Write([]byte(`{"ok":true}`))
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	keyName := "default"
	if ok, k := store.checkAPIKey(r); !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid or missing API key","type":"authentication_error"}}`))
		return
	} else if k != nil && k.Name != "" {
		keyName = k.Name
	}
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST only"}`, http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, `{"error":"read body"}`, http.StatusBadRequest)
		return
	}
	var o oaiRequest
	if err := json.Unmarshal(body, &o); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	approxPrompt := estimatePromptTokens(&o)
	upstreamModel, thinkingBudget := resolveUpstreamModel(o.Model)

	// Try every distinct active account (round-robin cursor; failed ones rotate
	// to the next account until the pool is exhausted).
	store.mu.Lock()
	nActive := 0
	for _, a := range store.Accounts {
		if a.Active {
			nActive++
		}
	}
	store.mu.Unlock()
	if nActive == 0 {
		http.Error(w, `{"error":{"message":"no active accounts configured"}}`, http.StatusServiceUnavailable)
		return
	}
	maxAttempts := nActive
	attempted := map[string]bool{}
	seen400Count := 0
	var lastCode int
	var lastErrBody string
	for attempt := 0; attempt < maxAttempts; attempt++ {
		acc := store.pickRoundRobin()
		if acc == nil {
			http.Error(w, `{"error":{"message":"no active accounts configured"}}`, http.StatusServiceUnavailable)
			return
		}
		if attempted[acc.ID] {
			// every active account already tried
			break
		}
		attempted[acc.ID] = true
		tok, err := store.ensureToken(acc)
		if err != nil {
			slogf("[attempt %d] %s token refresh failed: %v", attempt, acc.Email, err)
			store.markError(acc, "refresh: "+err.Error())
			continue
		}
		agBody := buildAGRequest(&o, upstreamModel, thinkingBudget, acc.ProjectID)
		url := agHost + "/v1internal:streamGenerateContent?alt=sse"
		if !o.Stream {
			url = agHost + "/v1internal:generateContent"
		}
		req, _ := http.NewRequest("POST", url, bytes.NewReader(agBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		// Wire-accurate identity (9router chunks/4953.js format:antigravity transport):
		// UA "antigravity/ide/2.1.1 darwin/arm64" + body {project, model, request{...}}.
		// Verified 200 against daily-cloudcode-pa on 2026-08-28 (GeminiCLI UA → 403 #3501).
		req.Header.Set("User-Agent", agUA)
		client := store.clientFor(acc)
		resp, err := client.Do(req)
		if err != nil {
			slogf("[attempt %d] %s upstream error: %v", attempt, acc.Email, err)
			store.markError(acc, err.Error())
			lastErrBody = err.Error()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			eb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			resp.Body.Close()
			lastCode, lastErrBody = resp.StatusCode, string(eb)
			if resp.StatusCode == 400 {
				_ = os.WriteFile("/tmp/last_ag_400_req.json", agBody, 0644)
				_ = os.WriteFile("/tmp/last_ag_400_err.json", eb, 0644)
			}
			slogf("[attempt %d] %s upstream %d: %.500s", attempt, acc.Email, resp.StatusCode, lastErrBody)

			// 404 NOT_FOUND = Model does not exist upstream on Antigravity.
			// Rotating accounts cannot fix an unknown model name.
			// Fail-fast immediately and do NOT taint account error status!
			if resp.StatusCode == http.StatusNotFound {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"error":{"message":"Model %q not found upstream on Antigravity (Requested entity was not found)","type":"invalid_request_error","code":"model_not_found"}}`, o.Model)
				return
			}

			store.markError(acc, fmt.Sprintf("%d: %.120s", resp.StatusCode, lastErrBody))

			isCapacityOverload := resp.StatusCode == 503 || resp.StatusCode == 529 ||
				strings.Contains(lastErrBody, "MODEL_CAPACITY_EXHAUSTED") ||
				strings.Contains(lastErrBody, "OVERLOADED_TOO_MANY_RETRIES_PER_REQUEST")

			// Retry strategy:
			// Capacity overload (503 or 400 MODEL_CAPACITY_EXHAUSTED):
			// Give Google's edge node time to cool down (3s -> 6s).
			if isCapacityOverload {
				backoffs := []time.Duration{3 * time.Second, 6 * time.Second}
				for bi, d := range backoffs {
					time.Sleep(d)
					req2, _ := http.NewRequest("POST", url, bytes.NewReader(agBody))
					req2.Header.Set("Content-Type", "application/json")
					req2.Header.Set("Authorization", "Bearer "+tok)
					req2.Header.Set("User-Agent", agUA)
					resp2, err2 := client.Do(req2)
					if err2 != nil {
						slogf("[retry %d] %s transport error: %v", bi+1, acc.Email, err2)
						continue
					}
					if resp2.StatusCode == http.StatusOK {
						slogf("[retry %d] %s RECOVERED after %s (capacity back)", bi+1, acc.Email, d)
						slogf("chat OK %s model=%s acc=%s attempt=%d(retry%d)", r.RemoteAddr, o.Model, acc.Email, attempt+1, bi+1)
						store.markUsed(acc)
						if o.Stream {
							w.Header().Set("Content-Type", "application/json")
							w.Header().Set("Cache-Control", "no-cache")
							flusher, ok2 := w.(http.Flusher)
							if !ok2 {
								resp2.Body.Close()
								http.Error(w, "stream unsupported", http.StatusInternalServerError)
								return
							}
							w.WriteHeader(http.StatusOK)
							tu, errT := sseTranslate(resp2.Body, o.Model, w, flusher, approxPrompt)
							resp2.Body.Close()
							if errT == nil && tu != nil && usageTracker != nil {
								usageTracker.Record(o.Model, acc.Email, keyName, tu)
							}
							return
						}
						nb2, _ := io.ReadAll(resp2.Body)
						resp2.Body.Close()
						tu, outB := nonStreamResponse(nb2, o.Model, approxPrompt)
						w.WriteHeader(http.StatusOK)
						w.Write(outB)
						if tu != nil && usageTracker != nil {
							usageTracker.Record(o.Model, acc.Email, keyName, tu)
						}
						return
					}
					eb2, _ := io.ReadAll(io.LimitReader(resp2.Body, 2048))
					resp2.Body.Close()
					lastCode, lastErrBody = resp2.StatusCode, string(eb2)
					slogf("[retry %d] %s still %d", bi+1, acc.Email, resp2.StatusCode)
					stillCapacity := resp2.StatusCode == 503 || resp2.StatusCode == 529 ||
						strings.Contains(lastErrBody, "MODEL_CAPACITY_EXHAUSTED") ||
						strings.Contains(lastErrBody, "OVERLOADED_TOO_MANY_RETRIES_PER_REQUEST")
					if !stillCapacity {
						break // different error class → leave to outer rotation
					}
				}
				continue // rotate to next account after same-account retries exhausted
			}

			// True 400 errors (schema/syntax): rotate to other accounts first.
			// Only surface if at least 4 accounts (or all active) consistently reject it.
			if resp.StatusCode == 400 && !isCapacityOverload {
				seen400Count++
				threshold := 4
				if nActive < threshold {
					threshold = nActive
				}
				if seen400Count >= threshold {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(resp.StatusCode)
					fmt.Fprintf(w, `{"error":{"message":"upstream 400 after retry: %.400s"}}`, lastErrBody)
					slogf("chat 400-surfaced %s model=%s (payload rejected by %d accounts)", r.RemoteAddr, o.Model, seen400Count)
					return
				}
			}
			continue
		}

		// Success — stream it out.
		store.markUsed(acc)
		slogf("chat OK %s model=%s acc=%s attempt=%d", r.RemoteAddr, o.Model, acc.Email, attempt+1)
		w.Header().Set("Content-Type", "application/json")
		if o.Stream {
			w.Header().Set("Cache-Control", "no-cache")
			flusher, ok := w.(http.Flusher)
			if !ok {
				resp.Body.Close()
				http.Error(w, "stream unsupported", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			tu, errT := sseTranslate(resp.Body, o.Model, w, flusher, approxPrompt)
			resp.Body.Close()
			if errT == nil && tu != nil && usageTracker != nil {
				usageTracker.Record(o.Model, acc.Email, keyName, tu)
			}
			return
		}
		nb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		tu, outB := nonStreamResponse(nb, o.Model, approxPrompt)
		w.WriteHeader(http.StatusOK)
		w.Write(outB)
		if tu != nil && usageTracker != nil {
			usageTracker.Record(o.Model, acc.Email, keyName, tu)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, `{"error":{"message":"all attempts failed","last_status":%d,"last_error":%.600s}}`, lastCode, lastErrBody)
}

// =====================================================================
// Admin API (token-protected) + models list
// =====================================================================

func adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := ""
		if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
			tok = strings.TrimPrefix(ah, "Bearer ")
		}
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		want := adminToken
		store.mu.Lock()
		if store.AdminToken != "" {
			want = store.AdminToken
		}
		store.mu.Unlock()
		if tok == "" || tok != want {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// checkAPIKey enforces the /v1 gate. Rules:
//   - no keys configured → open access (backward compat)
//   - keys configured → Bearer key required, must exist and not be disabled
//     (admin token also accepted). Usage counters updated on success.
func (s *Store) checkAPIKey(r *http.Request) (bool, *APIKey) {
	ah := r.Header.Get("Authorization")
	tok := ""
	if strings.HasPrefix(ah, "Bearer ") {
		tok = strings.TrimPrefix(ah, "Bearer ")
	}
	if tok == "" {
		tok = r.URL.Query().Get("key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.APIKeys) == 0 {
		return true, nil
	}
	admin := s.AdminToken
	for _, k := range s.APIKeys {
		if k.Key == tok && !k.Disabled {
			k.LastUsed = time.Now().UTC().Format(time.RFC3339)
			k.ReqCount++
			s.save()
			return true, k
		}
	}
	if admin != "" && tok == admin {
		return true, nil
	}
	pushLog("AUTH FAIL " + time.Now().UTC().Format("15:04:05") + " " + r.RemoteAddr + " key=" + fmt.Sprintf("%.8s...", tok))
	return false, nil
}

// handleAPIKeys: GET (list masked), POST (create {"name"} → returns full key ONCE),
// DELETE ?id= → remove, POST /action {"id","action":"disable|enable"}.
func handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	store.mu.Lock()
	defer store.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		out := []map[string]interface{}{}
		for _, k := range store.APIKeys {
			masked := k.Key
			if len(k.Key) > 12 {
				masked = k.Key[:9] + "..." + k.Key[len(k.Key)-4:]
			}
			out = append(out, map[string]interface{}{
				"id": k.ID, "name": k.Name, "key": masked, "createdAt": k.CreatedAt,
				"lastUsed": k.LastUsed, "requests": k.ReqCount, "disabled": k.Disabled,
			})
		}
		openAccess := len(store.APIKeys) == 0
		json.NewEncoder(w).Encode(map[string]interface{}{"keys": out, "openAccess": openAccess})
	case http.MethodPost:
		var in struct{ Name string `json:"name"` }
		json.NewDecoder(r.Body).Decode(&in)
		if strings.TrimSpace(in.Name) == "" {
			in.Name = "key-" + newID()[:4]
		}
		k := &APIKey{
			ID:        newID(),
			Name:      strings.TrimSpace(in.Name),
			Key:       "agk-" + newID() + newID(),
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		store.APIKeys = append(store.APIKeys, k)
		store.save()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": k.ID, "name": k.Name, "key": k.Key,
			"note": "simpan sekarang — key hanya ditampilkan penuh sekali ini",
		})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		for i, k := range store.APIKeys {
			if k.ID == id {
				store.APIKeys = append(store.APIKeys[:i], store.APIKeys[i+1:]...)
				store.save()
				w.Write([]byte(`{"ok":true}`))
				return
			}
		}
		http.Error(w, `{"error":"not found"}`, 404)
	}
}

func handleAPIKeyAction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Action string `json:"action"` // disable | enable
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad json"}`, 400)
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, k := range store.APIKeys {
		if k.ID == in.ID {
			switch in.Action {
			case "disable":
				k.Disabled = true
			case "enable":
				k.Disabled = false
			default:
				http.Error(w, `{"error":"unknown action"}`, 400)
				return
			}
			store.save()
			w.Write([]byte(`{"ok":true}`))
			return
		}
	}
	http.Error(w, `{"error":"not found"}`, 404)
}

func handleAccounts(w http.ResponseWriter, r *http.Request) {
	store.mu.Lock()
	defer store.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		// masked view
		type view struct {
			ID     string `json:"id"`
			Email  string `json:"email"`
			Active bool   `json:"active"`
			Proj   string `json:"projectId,omitempty"`
			Proxy  string `json:"proxyUrl,omitempty"`
			Reqs   int64  `json:"requests"`
			Errs   int64  `json:"errors"`
			LastE  string `json:"lastError,omitempty"`
			Exp    string `json:"tokenExpiresAt,omitempty"`
		}
		out := []view{}
		for _, a := range store.Accounts {
			rt := a.RefreshTok
			masked := ""
			if len(rt) > 10 {
				masked = rt[:4] + "..." + rt[len(rt)-4:]
			}
			out = append(out, view{a.ID, a.Email, a.Active, a.ProjectID, a.ProxyURL, a.ReqCount, a.ErrCount, a.LastErr, a.ExpiresAt})
			_ = masked
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"accounts": out})
	case http.MethodPost:
		var in struct {
			Email       string `json:"email"`
			RefreshTok  string `json:"refreshToken"`
			ProjectID   string `json:"projectId"`
			ProxyURL    string `json:"proxyUrl"`
			RefreshFile string `json:"refreshFile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if in.RefreshFile != "" {
			b, err := os.ReadFile(in.RefreshFile)
			if err != nil {
				http.Error(w, "refreshFile unreadable: "+err.Error(), 400)
				return
			}
			in.RefreshTok = strings.TrimSpace(string(b))
		}
		if in.RefreshTok == "" {
			http.Error(w, "refreshToken required", 400)
			return
		}
		var acc *Account
		for _, a := range store.Accounts {
			if in.Email != "" && a.Email == in.Email {
				acc = a
				break
			}
		}
		if acc == nil {
			acc = &Account{
				ID:     newID(),
				Email:  in.Email,
				Active: true,
			}
			store.Accounts = append(store.Accounts, acc)
		}
		acc.RefreshTok = in.RefreshTok
		if in.ProjectID != "" {
			acc.ProjectID = in.ProjectID
		}
		if in.ProxyURL != "" {
			acc.ProxyURL = in.ProxyURL
		}
		acc.Active = true
		acc.LastErr = ""
		acc.AccessTok = ""
		acc.ExpiresAt = ""
		store.save()
		json.NewEncoder(w).Encode(map[string]string{"id": acc.ID})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		for i, a := range store.Accounts {
			if a.ID == id {
				store.Accounts = append(store.Accounts[:i], store.Accounts[i+1:]...)
				store.save()
				w.Write([]byte(`{"ok":true}`))
				return
			}
		}
		http.Error(w, "not found", 404)
	}
}

func handleAccountAction(w http.ResponseWriter, r *http.Request) {
	// POST /admin/accounts/action {"id":"...","action":"enable|disable|test"}
	var in struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	a := store.byID(in.ID)
	if a == nil {
		http.Error(w, "not found", 404)
		return
	}
	switch in.Action {
	case "enable":
		store.mu.Lock()
		a.Active = true
		a.LastErr = ""
		store.save()
		store.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case "disable":
		store.mu.Lock()
		a.Active = false
		store.save()
		store.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	case "test":
		tok, err := store.ensureToken(a)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		code, body := probeModel(a, tok)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": code == 200, "status": code, "body": body})
	default:
		http.Error(w, "unknown action", 400)
	}
}

func handleAccountsBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST only"}`, http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "delete" | "enable" | "disable"
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if len(in.IDs) == 0 {
		http.Error(w, `{"error":"no ids provided"}`, http.StatusBadRequest)
		return
	}

	idMap := make(map[string]bool, len(in.IDs))
	for _, id := range in.IDs {
		idMap[id] = true
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	count := 0
	switch in.Action {
	case "delete":
		newAccounts := make([]*Account, 0, len(store.Accounts))
		for _, a := range store.Accounts {
			if idMap[a.ID] {
				count++
			} else {
				newAccounts = append(newAccounts, a)
			}
		}
		store.Accounts = newAccounts
		store.save()
	case "enable":
		for _, a := range store.Accounts {
			if idMap[a.ID] {
				a.Active = true
				a.LastErr = ""
				count++
			}
		}
		store.save()
	case "disable":
		for _, a := range store.Accounts {
			if idMap[a.ID] {
				a.Active = false
				count++
			}
		}
		store.save()
	default:
		http.Error(w, `{"error":"unknown action"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    true,
		"count": count,
	})
}

// probeModel sends a tiny generateContent to verify the account end-to-end.
func probeModel(a *Account, tok string) (int, string) {
	tiny := fmt.Sprintf(`{"project":%q,"model":"gemini-3.8-flash-tiered","request":{"contents":[{"role":"user","parts":[{"text":"ping"}]}],"generationConfig":{"maxOutputTokens":16}}}`, a.ProjectID)
	req, _ := http.NewRequest("POST", agHost+"/v1internal:generateContent", strings.NewReader(tiny))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", agUA)
	resp, err := store.clientFor(a).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return resp.StatusCode, string(b)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	store.mu.Lock()
	total := len(store.Accounts)
	active := 0
	for _, a := range store.Accounts {
		if a.Active {
			active++
		}
	}
	store.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"service": "agrouter", "accounts": total, "active": active,
		"time": time.Now().UTC().Format(time.RFC3339),
	})
}

// models catalogue — mirrors what the antigravity pool actually serves
const agQuotaHost = "https://cloudcode-pa.googleapis.com"

// quotaSnapshot is one account's per-model quota state.
type quotaSnapshot struct {
	Email     string                  `json:"email"`
	Active    bool                    `json:"active"`
	Tier      string                  `json:"tier,omitempty"`
	Error     string                  `json:"error,omitempty"`
	FetchedAt string                  `json:"fetchedAt"`
	Quotas    map[string]modelQuota   `json:"quotas"`
}

type modelQuota struct {
	RemainingPct float64 `json:"remainingPct"`
	ResetAt      string  `json:"resetAt,omitempty"`
}

var (
	quotaCacheMu   sync.Mutex
	quotaCache     []*quotaSnapshot
	quotaCacheTime time.Time
)

// fetchAccountQuota calls loadCodeAssist + fetchAvailableModels for one account.
func fetchAccountQuota(a *Account, tok string) (*quotaSnapshot, error) {
	snap := &quotaSnapshot{
		Email: a.Email, Active: a.Active,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Quotas:    map[string]modelQuota{},
	}
	client := store.clientFor(a)
	post := func(path string, body map[string]interface{}) (map[string]interface{}, error) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", agQuotaHost+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("User-Agent", agUA)
		req.Header.Set("X-Client-Name", "antigravity")
		req.Header.Set("X-Client-Version", "2.1.1")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%d: %.120s", resp.StatusCode, string(raw))
		}
		var out map[string]interface{}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return out, nil
	}

	// Tier/plan
	lca, err := post("/v1internal:loadCodeAssist", map[string]interface{}{
		"metadata": map[string]string{"ideType": "IDE_UNSPECIFIED", "platform": "PLATFORM_UNSPECIFIED", "pluginType": "GEMINI"},
		"mode":     1,
	})
	if err == nil {
		if tier, ok := lca["currentTier"].(map[string]interface{}); ok {
			snap.Tier, _ = tier["name"].(string)
		}
	}

	// Per-model quota
	fam, err := post("/v1internal:fetchAvailableModels", map[string]interface{}{
		"project": a.ProjectID,
	})
	if err != nil {
		return nil, err
	}
	models, _ := fam["models"].(map[string]interface{})
	for mid, v := range models {
		info, ok := v.(map[string]interface{})
		if !ok || info["isInternal"] == true {
			continue
		}
		qi, ok := info["quotaInfo"].(map[string]interface{})
		if !ok {
			continue
		}
		frac, _ := qi["remainingFraction"].(float64)
		reset, _ := qi["resetTime"].(string)
		if qi["remainingFraction"] == nil {
			// model listed but no quota bucket for this tier — not "0% left"
			snap.Quotas[mid] = modelQuota{RemainingPct: -1, ResetAt: reset}
			continue
		}
		snap.Quotas[mid] = modelQuota{RemainingPct: frac * 100, ResetAt: reset}
	}
	return snap, nil
}

// handleQuota refreshes (or serves cached) quota for all active accounts.
func handleQuota(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	quotaCacheMu.Lock()
	defer quotaCacheMu.Unlock()
	if !refresh && time.Since(quotaCacheTime) < 3*time.Minute && quotaCache != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"fetchedAt": quotaCacheTime.UTC().Format(time.RFC3339), "cached": true, "accounts": quotaCache})
		return
	}
	store.mu.Lock()
	var targets []*Account
	for _, a := range store.Accounts {
		if a.Active {
			targets = append(targets, a)
		}
	}
	store.mu.Unlock()

	results := make([]*quotaSnapshot, len(targets))
	for i, a := range targets {
		results[i] = &quotaSnapshot{Email: a.Email, Active: true, Error: "pending", Quotas: map[string]modelQuota{}}
	}

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, a := range targets {
		wg.Add(1)
		go func(i int, acc *Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tok, err := store.ensureToken(acc)
			if err != nil {
				results[i].Error = "refresh: " + err.Error()
				results[i].FetchedAt = time.Now().UTC().Format(time.RFC3339)
				return
			}
			snap, err := fetchAccountQuota(acc, tok)
			if err != nil {
				results[i].Error = err.Error()
				results[i].FetchedAt = time.Now().UTC().Format(time.RFC3339)
				return
			}
			snap.FetchedAt = time.Now().UTC().Format(time.RFC3339)
			results[i] = snap
		}(i, a)
	}
	wg.Wait()

	quotaCache = results
	quotaCacheTime = time.Now()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"fetchedAt": quotaCacheTime.UTC().Format(time.RFC3339), "cached": false, "accounts": results})
}

// ---------- GSuite OAuth Onboarding Job ----------
type GSuiteJob struct {
	mu        sync.Mutex
	Running   bool      `json:"running"`
	StartedAt string    `json:"startedAt,omitempty"`
	Total     int       `json:"total"`
	Success   int       `json:"success"`
	Failed    int       `json:"failed"`
	Current   string    `json:"current,omitempty"`
	Error     string    `json:"error,omitempty"`
	cmd       *exec.Cmd `json:"-"`
}

var gsuiteJob GSuiteJob

func handleGSuiteOnboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST only"}`, http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Accounts string `json:"accounts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}

	gsuiteJob.mu.Lock()
	if gsuiteJob.Running {
		gsuiteJob.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"Proses onboarding GSuite sedang berjalan. Pantau di console log."}`))
		return
	}

	// Parse lines: email|password, email:password, email\tpassword
	lines := strings.Split(in.Accounts, "\n")
	var validLines []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		var email, pass string
		if strings.Contains(l, "|") {
			p := strings.SplitN(l, "|", 2)
			email, pass = p[0], p[1]
		} else if strings.Contains(l, ":") {
			p := strings.SplitN(l, ":", 2)
			email, pass = p[0], p[1]
		} else if strings.Contains(l, "\t") {
			p := strings.SplitN(l, "\t", 2)
			email, pass = p[0], p[1]
		}
		email = strings.TrimSpace(email)
		pass = strings.TrimSpace(pass)
		if strings.Contains(email, "@") && pass != "" {
			validLines = append(validLines, email+"|"+pass)
		}
	}

	if len(validLines) == 0 {
		gsuiteJob.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"Tidak ada akun valid ditemukan. Format per baris: email|password"}`))
		return
	}

	scriptDir := getGSuiteDir()
	queueFile := filepath.Join(scriptDir, "accounts_onboard.txt")
	if err := os.WriteFile(queueFile, []byte(strings.Join(validLines, "\n")+"\n"), 0o600); err != nil {
		gsuiteJob.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"gagal tulis file antrean: %v"}`, err)
		return
	}

	gsuiteJob.Running = true
	gsuiteJob.StartedAt = time.Now().UTC().Format(time.RFC3339)
	gsuiteJob.Total = len(validLines)
	gsuiteJob.Success = 0
	gsuiteJob.Failed = 0
	gsuiteJob.Current = ""
	gsuiteJob.Error = ""
	gsuiteJob.mu.Unlock()

	slogf("[gsuite] Memulai onboarding %d akun GSuite dari dashboard...", len(validLines))

	go runGSuiteWorker(queueFile)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"total":   len(validLines),
		"message": fmt.Sprintf("Memulai onboarding %d akun. Pantau live log di console.", len(validLines)),
	})
}

func runGSuiteWorker(queueFile string) {
	adminTok := adminToken
	store.mu.Lock()
	if store.AdminToken != "" {
		adminTok = store.AdminToken
	}
	store.mu.Unlock()

	nodeBin, err := exec.LookPath("node")
	if err != nil {
		nodeBin = "/usr/local/bin/node"
	}

	scriptDir := getGSuiteDir()
	cmd := exec.Command(nodeBin, "ag_gsuite.js", "--file", "accounts_onboard.txt")
	cmd.Dir = scriptDir
	cmd.Env = append(os.Environ(), "AGROUTER_ADMIN_TOKEN="+adminTok)

	gsuiteJob.mu.Lock()
	gsuiteJob.cmd = cmd
	gsuiteJob.mu.Unlock()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slogf("[gsuite] pipe error: %v", err)
		gsuiteJob.mu.Lock()
		gsuiteJob.Running = false
		gsuiteJob.Error = err.Error()
		gsuiteJob.mu.Unlock()
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		slogf("[gsuite] start error: %v", err)
		gsuiteJob.mu.Lock()
		gsuiteJob.Running = false
		gsuiteJob.Error = err.Error()
		gsuiteJob.mu.Unlock()
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		slogf("%s", line)

		gsuiteJob.mu.Lock()
		if strings.Contains(line, "=== ") && strings.Contains(line, "@") {
			idx := strings.Index(line, "=== ")
			if idx != -1 {
				rem := line[idx+4:]
				if endIdx := strings.Index(rem, " ==="); endIdx != -1 {
					gsuiteJob.Current = strings.TrimSpace(rem[:endIdx])
				}
			}
		} else if strings.Contains(line, "OK id=") {
			gsuiteJob.Success++
		} else if strings.Contains(line, "FAIL:") {
			gsuiteJob.Failed++
		}
		gsuiteJob.mu.Unlock()
	}

	err = cmd.Wait()
	gsuiteJob.mu.Lock()
	gsuiteJob.Running = false
	gsuiteJob.Current = ""
	gsuiteJob.cmd = nil
	if err != nil {
		gsuiteJob.Error = err.Error()
	}
	success := gsuiteJob.Success
	failed := gsuiteJob.Failed
	total := gsuiteJob.Total
	gsuiteJob.mu.Unlock()

	slogf("[gsuite] Selesai onboarding (%d OK, %d Gagal dari %d akun).", success, failed, total)
}

func handleGSuiteStatus(w http.ResponseWriter, r *http.Request) {
	gsuiteJob.mu.Lock()
	defer gsuiteJob.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"running":   gsuiteJob.Running,
		"startedAt": gsuiteJob.StartedAt,
		"total":     gsuiteJob.Total,
		"success":   gsuiteJob.Success,
		"failed":    gsuiteJob.Failed,
		"current":   gsuiteJob.Current,
		"error":     gsuiteJob.Error,
	})
}

func handleGSuiteStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST only"}`, http.StatusMethodNotAllowed)
		return
	}
	gsuiteJob.mu.Lock()
	if !gsuiteJob.Running || gsuiteJob.cmd == nil || gsuiteJob.cmd.Process == nil {
		gsuiteJob.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "message": "tidak ada proses berjalan"})
		return
	}
	pid := gsuiteJob.cmd.Process.Pid
	gsuiteJob.cmd.Process.Kill()
	gsuiteJob.Running = false
	gsuiteJob.cmd = nil
	gsuiteJob.mu.Unlock()

	exec.Command("pkill", "-P", fmt.Sprintf("%d", pid)).Run()
	slogf("[gsuite] Onboarding dibatalkan oleh admin")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "message": "proses dibatalkan"})
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	models := []string{
		"gemini-3.8-flash-high", "gemini-3.8-flash-medium", "gemini-3.8-flash-low",
		"gemini-3.7-flash-high", "gemini-3.7-flash-medium", "gemini-3.7-flash-low",
		"gemini-3.6-flash-high", "gemini-3.6-flash-medium", "gemini-3.6-flash-low",
		"gemini-3.5-flash-low", "gemini-3.5-flash-extra-low",
		"gemini-3-flash-agent", "gemini-pro-agent", "gemini-3.1-pro-low",
		"claude-sonnet-4-6", "claude-opus-4-6-thinking", "gpt-oss-120b-medium",
	} // live-tested 2026-08-28/09-02: 3.8-tiered verified live (2026-09-02, not yet in fetchAvailableModels list but callable)
	data := []map[string]interface{}{}
	for _, m := range models {
		data = append(data, map[string]interface{}{"id": m, "object": "model", "owned_by": "antigravity"})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	admin := os.Getenv("AGR_ADMIN_TOKEN")
	if admin != "" {
		_ = admin // adminToken is a const; env override wired below via closure
	}

	listenAddr := getEnv("AGROUTER_LISTEN", defaultListenAddr)
	dataPath := getEnv("AGROUTER_DATA", defaultDataPath)
	usageFile := getEnv("AGROUTER_USAGE", "/root/agrouter-data/usage.jsonl")
	logFile := getEnv("AGROUTER_LOG", "/var/log/apps/agrouter.log")

	s, err := loadStore(dataPath)
	if err != nil {
		log.Fatalf("load store: %v", err)
	}
	s.ensurePerms()
	store = s

	var errU error
	usageTracker, errU = initUsageTracker(usageFile, logFile)
	if errU != nil {
		log.Printf("warning: init usage tracker: %v", errU)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/health", handleHealth)
	dash := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(dashboardHTML)
	}
	// Dashboard HTML is public (it only shows a login form until the browser
	// presents a valid token); every /admin/accounts* call stays token-gated.
	mux.HandleFunc("/admin", dash)
	mux.HandleFunc("/admin/", dash)
	mux.HandleFunc("/admin/accounts", adminAuth(handleAccounts))
	mux.HandleFunc("/admin/accounts/action", adminAuth(handleAccountAction))
	mux.HandleFunc("/admin/accounts/batch", adminAuth(handleAccountsBatch))
	mux.HandleFunc("/admin/quota", adminAuth(handleQuota))
	mux.HandleFunc("/admin/usage", adminAuth(handleUsage))
	mux.HandleFunc("/admin/keys", adminAuth(handleAPIKeys))
	mux.HandleFunc("/admin/keys/action", adminAuth(handleAPIKeyAction))
	mux.HandleFunc("/admin/logs", adminAuth(handleLogs))
	mux.HandleFunc("/admin/logs/clear", adminAuth(handleClearLogs))
	mux.HandleFunc("/admin/gsuite/onboard", adminAuth(handleGSuiteOnboard))
	mux.HandleFunc("/admin/gsuite/status", adminAuth(handleGSuiteStatus))
	mux.HandleFunc("/admin/gsuite/stop", adminAuth(handleGSuiteStop))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("agrouter listening on %s (accounts: %d)", listenAddr, len(store.Accounts))
	log.Fatal(srv.ListenAndServe())
}
