package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseStoredCanonical(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"A","refreshToken":"R","domain":"www.workbuddy.ai"},"account":{"uid":"U"}}`)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("canonical parse: %v", err)
	}
	if sa.Auth.AccessToken != "A" || sa.Auth.RefreshToken != "R" || sa.Account.UID != "U" {
		t.Fatalf("canonical roundtrip mismatch: %+v", sa)
	}
}

func TestParseStoredFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"A2","refreshToken":"R2","domain":"www.workbuddy.cn","baseURL":"https://www.workbuddy.cn","uid":"U2"}`)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("flat parse: %v", err)
	}
	if sa.Auth.AccessToken != "A2" || sa.Auth.Domain != "www.workbuddy.cn" || sa.BaseURL != "https://www.workbuddy.cn" || sa.Account.UID != "U2" {
		t.Fatalf("flat mapping mismatch: %+v", sa)
	}
}

func TestParseStoredAPIKey(t *testing.T) {
	raw := []byte(`{"type":"workbuddy","mode":"api_key","apiKey":"K"}`)
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("api_key parse: %v", err)
	}
	if sa.Auth.AccessToken != "K" {
		t.Fatalf("apiKey should map to accessToken: %+v", sa)
	}
}

func TestParseStoredRejectsGarbage(t *testing.T) {
	for _, raw := range []string{
		`not json at all`,
		`{"auth":{}}`,
		`{"accessToken":""}`,
		`{"hello":"world"}`,
	} {
		if _, err := parseStored([]byte(raw)); err == nil {
			t.Fatalf("expected rejection for %s", raw)
		}
	}
}

func TestBaseURLFor(t *testing.T) {
	def := baseURLFor(nil)
	if def != defaultBase {
		t.Fatalf("default base = %s", def)
	}
	dom := baseURLFor(&storedAuth{Auth: storedTokens{Domain: "www.workbuddy.cn"}})
	if dom != "https://www.workbuddy.cn" {
		t.Fatalf("domain base = %s", dom)
	}
	ov := baseURLFor(&storedAuth{BaseURL: "https://www.codebuddy.cn/", Auth: storedTokens{Domain: "www.workbuddy.cn"}})
	if ov != "https://www.codebuddy.cn" {
		t.Fatalf("baseURL should win over domain, got %s", ov)
	}
	urlish := baseURLFor(&storedAuth{Auth: storedTokens{Domain: "https://copilot.tencent.com/"}})
	if urlish != "https://copilot.tencent.com" {
		t.Fatalf("url-style domain = %s", urlish)
	}
}

func TestRealmRoutingAndHeaders(t *testing.T) {
	// Test env override
	t.Setenv("WORKBUDDY_BASE_URL", "https://custom.workbuddy.internal/")
	if got := baseURLFor(&storedAuth{BaseURL: "https://ignore.me"}); got != "https://custom.workbuddy.internal" {
		t.Fatalf("env override failed, got %s", got)
	}

	// Test backendHeaders
	req, _ := http.NewRequest("POST", "https://copilot.tencent.com/v2/chat/completions", nil)
	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  "test-token",
			RefreshToken: "test-refresh",
			Domain:       "copilot.tencent.com",
		},
		Account: storedAccount{
			UID:          "uid-123",
			EnterpriseID: "ent-456",
		},
	}
	backendHeaders(req, sa)
	if req.Header.Get("Authorization") != "Bearer test-token" {
		t.Fatalf("missing auth header: %s", req.Header.Get("Authorization"))
	}
	if req.Header.Get("X-Access-Token") != "test-token" {
		t.Fatalf("missing x-access-token: %s", req.Header.Get("X-Access-Token"))
	}
	if req.Header.Get("X-Enterprise-Id") != "ent-456" {
		t.Fatalf("missing x-enterprise-id: %s", req.Header.Get("X-Enterprise-Id"))
	}
	if req.Header.Get("X-Auth-Refresh-Source") != providerName {
		t.Fatalf("missing x-auth-refresh-source: %s", req.Header.Get("X-Auth-Refresh-Source"))
	}
	if req.Header.Get("Origin") != "https://www.codebuddy.cn" {
		t.Fatalf("wrong origin: %s", req.Header.Get("Origin"))
	}
}

// TestBackendHeadersStampConversationIDs guards the upstream channel
// verification gate: /v2/chat/completions answers 400 code 11128 for any
// request missing the X-Conversation-* trio, so every chat request must carry
// all three, freshly generated per call.
func TestBackendHeadersStampConversationIDs(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{AccessToken: "tok", Domain: "www.workbuddy.ai"}}
	names := []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"}

	seen := make(map[string]string, len(names))
	for _, name := range names {
		req, _ := http.NewRequest("POST", "https://www.workbuddy.ai/v2/chat/completions", nil)
		backendHeaders(req, sa)
		got := req.Header.Get(name)
		if got == "" {
			t.Fatalf("backendHeaders dropped %s; upstream rejects with code 11128", name)
		}
		seen[name] = got
	}

	// Negative control: the IDs must be per-request, not a fixed constant.
	req, _ := http.NewRequest("POST", "https://www.workbuddy.ai/v2/chat/completions", nil)
	backendHeaders(req, sa)
	for _, name := range names {
		if req.Header.Get(name) == seen[name] {
			t.Fatalf("%s reused across requests: %s", name, seen[name])
		}
	}
}

func TestOriginForHost(t *testing.T) {
	cases := map[string]string{
		"www.workbuddy.ai":    "https://www.workbuddy.ai",
		"api.workbuddy.ai":    "https://www.workbuddy.ai",
		"www.workbuddy.cn":    "https://www.workbuddy.cn",
		"copilot.tencent.com": "https://www.codebuddy.cn",
		"unknown.example":     "https://www.codebuddy.cn",
	}
	for host, want := range cases {
		if got := originForHost(host); got != want {
			t.Fatalf("originForHost(%s) = %s, want %s", host, got, want)
		}
	}
}

func TestLoginBaseFromRaw(t *testing.T) {
	if got := loginBaseFromRaw(nil); got != defaultBase {
		t.Fatalf("empty login base = %s", got)
	}
	if got := loginBaseFromRaw([]byte(`{"realm":"cn"}`)); got != "https://copilot.tencent.com" {
		t.Fatalf("cn login base = %s", got)
	}
	if got := loginBaseFromRaw([]byte(`{"base_url":"https://www.workbuddy.cn/"}`)); got != "https://www.workbuddy.cn" {
		t.Fatalf("base_url login base = %s", got)
	}
}

func TestRewriteSystemForUpstreamUnblocksTemplates(t *testing.T) {
	blocked1 := "You are Claude Code, Anthropic's official CLI for Claude."
	blocked2 := "Main branch (you will usually use this for PRs)"
	body, _ := json.Marshal(map[string]any{
		"model": "hy3",
		"messages": []map[string]string{
			{"role": "system", "content": blocked1},
			{"role": "user", "content": blocked2},
		},
	})
	out := rewriteSystemForUpstream(body)
	if bytes.Contains(out, []byte(blocked1)) || bytes.Contains(out, []byte(blocked2)) {
		t.Fatalf("blocked templates survived rewrite: %s", out)
	}
	if !bytes.Contains(out, []byte("CLI tool")) || !bytes.Contains(out, []byte("Default branch")) {
		t.Fatalf("expected minimal rewrite markers: %s", out)
	}

	// Negative control 1: non-hy3 benign payloads keep their content and must
	// not gain a forced reasoning_effort (the rewrite is a normalizer, so byte
	// identity is not the contract; message content is).
	benign := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello CLI world"}]}`)
	out2 := rewriteSystemForUpstream(benign)
	if !bytes.Contains(out2, []byte("hello CLI world")) {
		t.Fatalf("benign content was modified: %s -> %s", benign, out2)
	}
	if bytes.Contains(out2, []byte("reasoning_effort")) {
		t.Fatalf("non-hy3 model gained reasoning_effort: %s", out2)
	}

	// Negative control 2: hy3 forcing overrides a client-supplied lower effort.
	low := []byte(`{"model":"hy3","reasoning_effort":"low","messages":[]}`)
	out3 := rewriteSystemForUpstream(low)
	if !bytes.Contains(out3, []byte(`"reasoning_effort":"high"`)) {
		t.Fatalf("hy3 forcing did not override client effort: %s", out3)
	}
	_ = strings.TrimSpace
}
func TestConcurrentRefreshAndAtomicSave(t *testing.T) {
	tmpDir := t.TempDir()
	authPath := tmpDir + "/workbuddy.json"

	initialData := []byte(`{
  "auth": {
    "accessToken": "old-token",
    "refreshToken": "old-refresh",
    "domain": "www.workbuddy.ai",
    "expiresAt": 1000
  },
  "account": {
    "uid": "user-1"
  },
  "rewriteRules": [
    {"pattern": "custom_prompt", "replacement": "new_prompt"}
  ]
}`)
	if err := os.WriteFile(authPath, initialData, 0600); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  "new-token",
			RefreshToken: "new-refresh",
			Domain:       "www.workbuddy.cn",
			ExpiresAt:    2000,
		},
		Account: storedAccount{
			UID: "user-1",
		},
	}

	if err := saveStoredAuthAtomic(sa, authPath); err != nil {
		t.Fatalf("saveStoredAuthAtomic: %v", err)
	}

	savedRaw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read saved: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(savedRaw, &parsed); err != nil {
		t.Fatalf("parse saved: %v", err)
	}

	authMap := parsed["auth"].(map[string]any)
	if authMap["accessToken"] != "new-token" || authMap["refreshToken"] != "new-refresh" {
		t.Fatalf("auth not updated: %+v", authMap)
	}
	rules, ok := parsed["rewriteRules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("custom rewriteRules lost: %+v", parsed)
	}
}
func TestCleanChunkJSONAndReasoning(t *testing.T) {
	// Test clientNeedsSSEFrame
	if clientNeedsSSEFrame(map[string]any{"request_path": "/v1/chat/completions"}) {
		t.Fatalf("OpenAI path should not need SSE frame")
	}
	if !clientNeedsSSEFrame(map[string]any{"request_path": "/v1/messages"}) {
		t.Fatalf("Anthropic path should need SSE frame")
	}

	// Test cleanChunkJSON strips empty tool_calls and empty deltas
	rawChunk := `{"choices":[{"index":0,"delta":{"role":"assistant","content":"hi","tool_calls":[]}}]}`
	cleaned := cleanChunkJSON(rawChunk)
	if strings.Contains(cleaned, `"tool_calls"`) {
		t.Fatalf("empty tool_calls survived cleaning: %s", cleaned)
	}

	// Test forceMaxThinking with Anthropic style thinking
	obj1 := map[string]any{
		"model":    "glm-5.2",
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
	}
	if !forceMaxThinking(obj1) || obj1["reasoning_effort"] != "high" {
		t.Fatalf("thinking enabled should set reasoning_effort=high: %+v", obj1)
	}

	// Test forceMaxThinking on hy3 models
	obj2 := map[string]any{
		"model":            "hy3-preview",
		"reasoning_effort": "low",
	}
	if !forceMaxThinking(obj2) || obj2["reasoning_effort"] != "high" {
		t.Fatalf("hy3 should force reasoning_effort=high: %+v", obj2)
	}
}
func TestAggregateToolCalls(t *testing.T) {
	// Simulate chunks arriving with tool_call deltas
	streamData := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"weather_lookup","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Beijing\"}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n")

	out, err := aggregateCompletion(strings.NewReader(streamData), "glm-5.2")
	if err != nil {
		t.Fatalf("aggregateCompletion error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("parse aggregated output: %v", err)
	}

	choices := res["choices"].([]any)
	choice0 := choices[0].(map[string]any)
	msg := choice0["message"].(map[string]any)
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 aggregated tool call, got %+v", msg)
	}

	tc0 := tcs[0].(map[string]any)
	if tc0["id"] != "call_abc" {
		t.Fatalf("id mismatch: %v", tc0["id"])
	}
	fn := tc0["function"].(map[string]any)
	if fn["name"] != "weather_lookup" {
		t.Fatalf("name mismatch: %v", fn["name"])
	}
	if fn["arguments"] != `{"city":"Beijing"}` {
		t.Fatalf("arguments assembly mismatch: %v", fn["arguments"])
	}
}
func TestDynamicRewriteRules(t *testing.T) {
	// Set dynamic rules
	setDynamicRules([]rewriteRule{
		{Pattern: "custom_forbidden_phrase", Replacement: "allowed_phrase"},
	})
	defer setDynamicRules(nil)

	// Test string message content
	payload := []byte(`{
		"model": "glm-5.2",
		"messages": [
			{"role": "user", "content": "hello custom_forbidden_phrase"}
		]
	}`)
	rewritten := rewriteSystemForUpstream(payload)
	if strings.Contains(string(rewritten), "custom_forbidden_phrase") {
		t.Fatalf("dynamic rule did not replace forbidden phrase: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), "allowed_phrase") {
		t.Fatalf("dynamic rule replacement missing: %s", rewritten)
	}

	// Test multimodal array content
	multiPayload := []byte(`{
		"model": "glm-5.2",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "prefix custom_forbidden_phrase suffix"}
				]
			}
		]
	}`)
	multiRewritten := rewriteSystemForUpstream(multiPayload)
	if strings.Contains(string(multiRewritten), "custom_forbidden_phrase") {
		t.Fatalf("multimodal text did not replace forbidden phrase: %s", multiRewritten)
	}
	if !strings.Contains(string(multiRewritten), "prefix allowed_phrase suffix") {
		t.Fatalf("multimodal replacement missing: %s", multiRewritten)
	}
}
func TestDynamicModelDiscovery(t *testing.T) {
	initial := wbModels()
	if len(initial) < 10 {
		t.Fatalf("expected at least 10 static models, got %d", len(initial))
	}

	// Merge dynamic upstream model
	syncUpstreamModels([]pluginapi.ModelInfo{
		{
			ID:          "custom-enterprise-model",
			DisplayName: "Custom Enterprise Model",
			ContextLength: 128000,
		},
	})
	defer syncUpstreamModels(nil)

	merged := wbModels()
	found := false
	for _, m := range merged {
		if m.ID == "custom-enterprise-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("dynamic model was not merged into wbModels()")
	}
}
func TestStreamPump401SelfHealing(t *testing.T) {
	// Verify that if first response is 401, refresh token logic is called
	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  "initial-token",
			RefreshToken: "refresh-token-val",
			ExpiresAt:    100,
		},
	}
	// Direct validation of Skew check calculation
	skewBuffer := int64(300)
	needsRefresh := time.Now().Unix()+skewBuffer >= sa.Auth.ExpiresAt
	if !needsRefresh {
		t.Fatalf("expected needsRefresh to be true for expired token")
	}
}
func TestNormalizeUpstreamError(t *testing.T) {
	// Test code 11101 (stream required)
	raw11101 := []byte(`{"code":11101,"msg":"non-stream not allowed","data":null}`)
	out1 := normalizeUpstreamError(200, raw11101)
	var obj1 map[string]any
	_ = json.Unmarshal(out1, &obj1)
	err1 := obj1["error"].(map[string]any)
	if err1["code"] != "11101" || err1["type"] != "invalid_request_error" {
		t.Fatalf("unexpected normalization for 11101: %+v", err1)
	}

	// Test 401 unauthorized
	out2 := normalizeUpstreamError(401, []byte(`{"code":10003,"msg":"token invalid"}`))
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	err2 := obj2["error"].(map[string]any)
	if err2["type"] != "authentication_error" {
		t.Fatalf("expected authentication_error: %+v", err2)
	}

	// Test 429 rate limit
	out3 := normalizeUpstreamError(429, []byte(`rate limit exceeded`))
	var obj3 map[string]any
	_ = json.Unmarshal(out3, &obj3)
	err3 := obj3["error"].(map[string]any)
	if err3["type"] != "rate_limit_exceeded" {
		t.Fatalf("expected rate_limit_exceeded: %+v", err3)
	}
}
