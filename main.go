// Package main implements the WorkBuddy AI CLIProxyAPI dynamic plugin.
//
// workbuddy wraps the model backend behind WorkBuddy AI.app (default realm
// www.workbuddy.ai; CN realms reachable via the credential domain / baseURL
// fields) as a cliproxy provider: it performs the /v2/plugin login flow,
// refreshes access tokens, imports desktop credentials and forwards
// OpenAI-compatible chat completion requests to the upstream
// /v2/chat/completions endpoint.
//
// Architecture and C ABI follow workbuddy-cliproxy (MIT, lovingfish), itself a
// clean-room rebuild of Sliverkiss/cpa-plugin's workbuddy plugin; the desktop
// credential import pattern follows Aho1ic/mimo-cpa (MIT); realm facts for the
// global WorkBuddy endpoint family cross-checked against aiden-git/wb2cpa (MIT).
// Built with -buildmode=c-shared exporting the cliproxy C ABI entry points.

package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

// Wrappers so Go can invoke the host function-pointer table via cgo. The host
// API captured at init is used to push streaming chunks back asynchronously.
static int wb_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}
static void wb_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	providerName  = "workbuddy"
	authFileName  = "workbuddy.json"

	// defaultBase is the WorkBuddy AI.app international realm. CN accounts are
	// reached by setting "domain" or "baseURL" in the credential JSON.
	defaultBase = "https://www.workbuddy.ai"
	clientUA    = "CLI/2.63.2 CodeBuddy/2.63.2"

	pathAuthState    = "/v2/plugin/auth/state?platform=CLI"
	pathLoginAcct    = "/v2/plugin/login/account?state="
	pathAuthToken    = "/v2/plugin/auth/token?state="
	pathTokenRefresh = "/v2/plugin/auth/token/refresh"
	pathChat         = "/v2/chat/completions"
	pathModels       = "/v2/enterprises/personal/models"

	loginTTL = 5 * time.Minute
)

// loginCtx holds the cookie-affined HTTP client for one in-flight login flow.
// CodeBuddy associates the browser login with the state issued at auth/state,
// so we must reuse the same cookie jar across the state request and the polls.
type loginCtx struct {
	client  *http.Client
	expires time.Time
	base    string
}

var (
	hostAPI        *C.cliproxy_host_api // captured at init, used for async host calls
	loginStates    sync.Map             // state(string) -> *loginCtx
	httpClientOnce sync.Once
	sharedClient   *http.Client
	refreshMu      sync.Mutex           // serializes token refreshes and auth saves
)
// pluginVersion is injected at link time for release builds:
//
//	-ldflags "-X main.pluginVersion=0.2.0"
var pluginVersion = "0.2.0"

func main() {}

// -----------------------------------------------------------------------------
// C ABI exports
// -----------------------------------------------------------------------------

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// -----------------------------------------------------------------------------
// Host calls (async streaming)
// -----------------------------------------------------------------------------

// hostCall invokes a host RPC method via the function-pointer table captured
// at init. Used to push stream chunks back asynchronously (host.stream.emit /
// host.stream.close).
func hostCall(method string, request []byte) ([]byte, error) {
	if hostAPI == nil || hostAPI.call == nil {
		return nil, fmt.Errorf("host API unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cReq unsafe.Pointer
	var reqLen C.size_t
	if len(request) > 0 {
		cReq = C.CBytes(request)
		defer C.free(cReq)
		reqLen = C.size_t(len(request))
	}
	var resp C.cliproxy_buffer
	rc := C.wb_call_host(hostAPI, cMethod, (*C.uint8_t)(cReq), reqLen, &resp)
	var out []byte
	if resp.ptr != nil && resp.len > 0 {
		out = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil && hostAPI.free_buffer != nil {
		C.wb_free_host_buffer(hostAPI, resp.ptr, resp.len)
	}
	if rc != 0 {
		return out, fmt.Errorf("host call %s returned %d", method, int(rc))
	}
	return out, nil
}

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := hostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	errJSON, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	_ = streamEmit(streamID, errJSON)
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID})
	_, _ = hostCall(pluginabi.MethodHostStreamClose, body)
}

// -----------------------------------------------------------------------------
// RPC dispatch
// -----------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return okEnvelope(wbRegistration())
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: wbModels()})
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// -----------------------------------------------------------------------------
// Registration & models
// -----------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
}

type streamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

func wbRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             providerName,
			Version:          pluginVersion,
			Author:           "workbuddy-CPA",
			GitHubRepository: "https://github.com/lovingfish/workbuddy-cliproxy",
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

var (
	dynamicModelsMu sync.RWMutex
	dynamicModels   []pluginapi.ModelInfo
)

func wbModels() []pluginapi.ModelInfo {
	const maxCompletionTokens int64 = 8192
	specs := []struct {
		id            string
		name          string
		contextLength int64
	}{
		{"deepseek-v4.1-flash", "Deepseek-V4.1-Flash", 131072},
		{"deepseek-v4.1-flash-sg", "Deepseek-V4.1-Flash (SG)", 131072},
		{"deepseek-v4-flash", "DeepSeek V4 Flash", 1000000},
		{"deepseek-v4-pro", "DeepSeek V4 Pro", 1000000},
		{"glm-5.3", "GLM-5.3", 1000000},
		{"glm-5.3-flash", "GLM-5.3-Flash", 1000000},
		{"glm-5.2", "GLM-5.2", 1000000},
		{"glm-5.1", "GLM-5.1", 131072},
		{"glm-5v-turbo", "GLM-5V Turbo", 131072},
		{"kimi-k3", "Kimi K3", 262144},
		{"kimi-k2.8-preview", "Kimi K2.8 Preview", 262144},
		{"kimi-k2.7", "Kimi K2.7", 262144},
		{"kimi-k2.6", "Kimi K2.6", 262144},
		{"minimax-m3", "MiniMax M3", 204800},
		{"minimax-m3-pay", "MiniMax M3 Pay", 204800},
		{"hy4-preview", "Hy4 Preview", 262144},
		{"hy4-preview-f", "Hy4 Preview F", 262144},
		{"hy3", "Hy3", 262144},
		{"hy3-preview", "Hy3 Preview", 262144},
		{"hy3-preview-agent", "Hy3 Preview Agent", 262144},
	}
	baseModels := make([]pluginapi.ModelInfo, 0, len(specs))
	seen := make(map[string]bool)
	for _, m := range specs {
		seen[m.id] = true
		baseModels = append(baseModels, pluginapi.ModelInfo{
			ID:                         m.id,
			Object:                     "model",
			OwnedBy:                    providerName,
			DisplayName:                m.name,
			Name:                       m.id,
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              m.contextLength,
			MaxCompletionTokens:        maxCompletionTokens,
			UserDefined:                true,
		})
	}

	dynamicModelsMu.RLock()
	extra := dynamicModels
	dynamicModelsMu.RUnlock()

	for _, m := range extra {
		if !seen[m.ID] {
			seen[m.ID] = true
			baseModels = append(baseModels, m)
		}
	}
	return baseModels
}

// syncUpstreamModels merges dynamic models fetched from upstream /v2/enterprises/personal/models.
func syncUpstreamModels(models []pluginapi.ModelInfo) {
	dynamicModelsMu.Lock()
	defer dynamicModelsMu.Unlock()
	dynamicModels = models
}

// -----------------------------------------------------------------------------
// Auth data shapes (matches persisted workbuddy.json)
// -----------------------------------------------------------------------------

// storedAuth is the on-disk shape of a workbuddy credential.
type storedAuth struct {
	Auth         storedTokens  `json:"auth"`
	Account      storedAccount `json:"account"`
	BaseURL      string        `json:"baseURL,omitempty"`
	RewriteRules []rewriteRule `json:"rewriteRules,omitempty"`
}

type rewriteRule struct {
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
}

type storedTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
}

type storedAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// apiEnvelope is the generic {code,msg,data} wrapper used by every CodeBuddy API.
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
}

type accountData struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type authStateData struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	raw = normalizeStoredShape(raw)
	var sa storedAuth
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	if sa.Auth.AccessToken == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &sa, nil
}


// baseURLFor resolves the upstream base URL for a stored credential with 4-tier precedence:
// 1. Environment variable WORKBUDDY_BASE_URL
// 2. Explicit sa.BaseURL
// 3. Normalized sa.Auth.Domain
// 4. Default fallback: https://www.workbuddy.ai
func baseURLFor(sa *storedAuth) string {
	if envBase := strings.TrimSpace(os.Getenv("WORKBUDDY_BASE_URL")); envBase != "" {
		return strings.TrimRight(envBase, "/")
	}
	if sa != nil {
		if sa.BaseURL != "" {
			return strings.TrimRight(sa.BaseURL, "/")
		}
		if d := normalizeHost(sa.Auth.Domain); d != "" {
			return "https://" + d
		}
	}
	return defaultBase
}

// normalizeHost turns a stored domain field (host or URL) into a bare host.
func normalizeHost(d string) string {
	d = strings.TrimSpace(d)
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	d = strings.TrimRight(d, "/")
	if i := strings.Index(d, "/"); i >= 0 {
		d = d[:i]
	}
	return d
}

// originForHost maps an upstream host to the web origin expected in the
// Origin/Referer headers for that realm.
func originForHost(host string) string {
	h := normalizeHost(host)
	switch {
	case strings.HasSuffix(h, "workbuddy.ai"):
		return "https://www.workbuddy.ai"
	case strings.HasSuffix(h, "workbuddy.cn"):
		return "https://www.workbuddy.cn"
	default:
		return "https://www.codebuddy.cn"
	}
}

// loginBaseFromRaw picks the login-flow base URL from an auth.login_start
// request. Recognized optional fields: "base_url"/"baseURL" (full URL) or
// "realm" ("global" | "cn" | "workbuddy.cn"). Empty input selects the
// WorkBuddy AI.app international default.
func loginBaseFromRaw(raw []byte) string {
	var probe struct {
		BaseURL string `json:"base_url"`
		BaseURL2 string `json:"baseURL"`
		Realm   string `json:"realm"`
	}
	_ = json.Unmarshal(raw, &probe)
	if probe.BaseURL != "" {
		return strings.TrimRight(probe.BaseURL, "/")
	}
	if probe.BaseURL2 != "" {
		return strings.TrimRight(probe.BaseURL2, "/")
	}
	switch strings.ToLower(strings.TrimSpace(probe.Realm)) {
	case "cn", "codebuddy":
		return "https://copilot.tencent.com"
	case "workbuddy.cn":
		return "https://www.workbuddy.cn"
	default:
		return defaultBase
	}
}

// normalizeStoredShape maps accepted credential shapes onto storedAuth JSON:
// the canonical {"auth":{...},"account":{...}} passes through unchanged, and
// flat shapes ({"accessToken":...} / {"auth":{"accessToken":...}} with legacy
// account wrapper / {"mode":"api_key","apiKey":...}) are wrapped into it.
// Unknown shapes pass through untouched so strict parsing reports the error.
func normalizeStoredShape(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if authRaw, ok := m["auth"]; ok {
		var tok map[string]json.RawMessage
		if err := json.Unmarshal(authRaw, &tok); err == nil {
			if _, has := tok["accessToken"]; has {
				return raw
			}
			if _, has := tok["refreshToken"]; has {
				return raw
			}
		}
		return raw
	}
	var flat struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		APIKey       string `json:"apiKey"`
		Mode         string `json:"mode"`
		Domain       string `json:"domain"`
		BaseURL      string `json:"baseURL"`
		BaseURL2     string `json:"base_url"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(raw, &flat); err != nil {
		return raw
	}
	access := flat.AccessToken
	if access == "" && flat.APIKey != "" {
		access = flat.APIKey
	}
	if access == "" && flat.RefreshToken == "" {
		return raw
	}
	base := flat.BaseURL
	if base == "" {
		base = flat.BaseURL2
	}
	wrapped := storedAuth{
		Auth: storedTokens{
			AccessToken:  access,
			RefreshToken: flat.RefreshToken,
			Domain:       flat.Domain,
		},
		Account: storedAccount{
			UID:          flat.UID,
			EnterpriseID: flat.EnterpriseID,
			Nickname:     flat.Nickname,
		},
		BaseURL: base,
	}
	out, err := json.Marshal(wrapped)
	if err != nil {
		return raw
	}
	return out
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		jar, _ := cookiejar.New(nil)
		sharedClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 5,
			},
			Jar: jar,
		}
	})
	return sharedClient
}

// newLoginClient builds an isolated client with its own cookie jar so that the
// browser login for one state can never leak into another.
func newLoginClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: sharedHTTPClient().Transport,
		Jar:       jar,
	}
}

func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originForHost(req.URL.Host)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// backendHeaders applies auth-derived headers to a chat completion request.
// Empty fields are signalled via the X-No-* convention used by CodeBuddy.
func backendHeaders(req *http.Request, sa *storedAuth) {
	commonHeaders(req)
	conversationHeaders(req)
	req.Header.Set("X-Auth-Refresh-Source", providerName)
	if sa != nil && sa.Auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
		req.Header.Set("X-Access-Token", sa.Auth.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if sa != nil && sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if sa != nil && sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if sa != nil && sa.Auth.RefreshToken != "" {
		req.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
	}
	if sa != nil && sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	req.Header.Set("X-Product", "SaaS")
}

// conversationHeaders stamps the per-request CodeBuddy CLI conversation
// identity. Upstream gates /v2/chat/completions on channel verification: a
// request carrying only auth headers is rejected with HTTP 400
// {"code":11128,"msg":"request illegal"} and displayTips "This call failed
// channel verification, possibly because it was not sent from an official
// client". All three IDs must be present — dropping any one of them restores
// the block — while the official client regenerates them per request, so fresh
// UUIDs per call are what upstream expects.
func conversationHeaders(req *http.Request) {
	req.Header.Set("X-Conversation-ID", newUUIDv4())
	req.Header.Set("X-Conversation-Request-ID", newUUIDv4())
	req.Header.Set("X-Conversation-Message-ID", newUUIDv4())
}

// newUUIDv4 returns a random RFC 4122 version 4 UUID. The upstream only checks
// that the conversation IDs exist, but unique values keep replayed requests
// indistinguishable from genuine CLI traffic.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x-%016x", time.Now().UnixNano(), os.Getpid())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// doJSON sends method to fullURL with the given headers, parses the {code,msg,data}
// envelope, and returns the inner data payload. httpStatus is the upstream code.
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// Auth handlers
// -----------------------------------------------------------------------------

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		// Not a workbuddy credential; let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    toAuthData(sa),
	})
}

func toAuthData(sa *storedAuth) pluginapi.AuthData {
	storage, _ := json.Marshal(sa)
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          providerName,
		FileName:    authFileName,
		Label:       "WorkBuddy",
		StorageJSON: storage,
		Metadata:    map[string]any{"type": providerName},
	}
}

func handleStartLogin(raw []byte) ([]byte, error) {
	base := loginBaseFromRaw(raw)
	client := newLoginClient()
	data, _, err := doJSON(client, http.MethodPost, base+pathAuthState, nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st authStateData
	_ = json.Unmarshal(data, &st)
	if st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl")
	}
	loginStates.Store(st.State, &loginCtx{client: client, expires: time.Now().Add(loginTTL), base: base})
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       st.AuthURL,
		State:     st.State,
		ExpiresAt: time.Now().Add(loginTTL).UTC(),
	})
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginStates.Load(state)
	if !ok {
		return nil, fmt.Errorf("poll: unknown state (restart login)")
	}
	lc := v.(*loginCtx)
	if time.Now().After(lc.expires) {
		loginStates.Delete(state)
		return nil, fmt.Errorf("poll: login expired")
	}

	// Single-shot poll per RPC: the host drives the polling cadence.
	// auth/token is the authoritative login-status endpoint: the application
	// layer returns code 11217 ("login ing") while pending, and code 0 with the
	// token bundle once complete. login/account sits behind the openresty gateway
	// and is rejected (401) until login finishes, so probe token first and only
	// fetch account once we hold a bearer.
	tokRaw, _, errTok := doJSON(lc.client, http.MethodGet, lc.base+pathAuthToken+state, nil, nil)
	if errTok != nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	var tok tokenData
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}

	var acct accountData
	acctHeaders := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(lc.client, http.MethodGet, lc.base+pathLoginAcct+state, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
			Domain:       tok.Domain,
		},
		Account: storedAccount{
			UID:          acct.UID,
			EnterpriseID: acct.EnterpriseID,
			Nickname:     acct.Nickname,
		},
	}
	loginStates.Delete(state)
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}

	refreshMu.Lock()
	defer refreshMu.Unlock()

	if err := refreshAuthTokenLocked(sa); err != nil {
		return nil, err
	}

	_ = saveStoredAuthAtomic(sa, authFileName)
	return okEnvelope(pluginapi.AuthRefreshResponse{Auth: toAuthData(sa)})
}

// refreshAuthTokenLocked executes /v2/plugin/auth/token/refresh. Caller must hold refreshMu.
func refreshAuthTokenLocked(sa *storedAuth) error {
	if sa == nil {
		return fmt.Errorf("nil stored auth")
	}
	headers := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("X-Refresh-Token", sa.Auth.RefreshToken)
		if sa.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		}
		r.Header.Set("X-Auth-Refresh-Source", providerName)
	}
	data, status, err := doJSON(sharedHTTPClient(), http.MethodPost, baseURLFor(sa)+pathTokenRefresh, headers, nil)
	if err != nil {
		if status >= 400 {
			return fmt.Errorf("refresh rejected (HTTP %d)", status)
		}
		return fmt.Errorf("refresh: %w", err)
	}
	var tok tokenData
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		sa.Auth.Domain = tok.Domain
	}
	sa.Auth.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	return nil
}

// saveStoredAuthAtomic performs a deep merge of updated tokens into path and atomically renames the temp file.
func saveStoredAuthAtomic(sa *storedAuth, path string) error {
	if sa == nil || path == "" {
		return nil
	}
	var existing map[string]any
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &existing)
	}
	if existing == nil {
		existing = make(map[string]any)
	}

	authMap, _ := existing["auth"].(map[string]any)
	if authMap == nil {
		authMap = make(map[string]any)
	}
	authMap["accessToken"] = sa.Auth.AccessToken
	if sa.Auth.RefreshToken != "" {
		authMap["refreshToken"] = sa.Auth.RefreshToken
	}
	if sa.Auth.Domain != "" {
		authMap["domain"] = sa.Auth.Domain
	}
	authMap["expiresAt"] = sa.Auth.ExpiresAt
	existing["auth"] = authMap

	accountMap, _ := existing["account"].(map[string]any)
	if accountMap == nil {
		accountMap = make(map[string]any)
	}
	if sa.Account.UID != "" {
		accountMap["uid"] = sa.Account.UID
	}
	if sa.Account.EnterpriseID != "" {
		accountMap["enterpriseId"] = sa.Account.EnterpriseID
	}
	if sa.Account.Nickname != "" {
		accountMap["nickname"] = sa.Account.Nickname
	}
	existing["account"] = accountMap

	if sa.BaseURL != "" {
		existing["baseURL"] = sa.BaseURL
	}

	raw, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// -----------------------------------------------------------------------------
// Executor handlers
// -----------------------------------------------------------------------------

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	// CodeBuddy rejects non-stream requests (code 11101), so always stream
	// upstream and fold the chunks into a single chat.completion object.
	body := rewriteSystemForUpstream(forceStreamBody(req.Payload, req.OriginalRequest))
	httpReq, err := http.NewRequest(http.MethodPost, baseURLFor(sa)+pathChat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, sa)
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(payload), 200))
	}
	completion, err := aggregateCompletion(resp.Body, req.Model)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// executorStreamRequest wraps the host's executor.execute_stream RPC: the
// ExecutorRequest plus the async stream id the host uses to receive chunks.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	body = rewriteSystemForUpstream(body)

	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	// No async stream id → fall back to synchronous chunk collection.
	if req.StreamID == "" {
		chunks, errCollect := collectUpstreamStream(body, sa, sseFramed)
		if errCollect != nil {
			return nil, errCollect
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async: return immediately with empty chunks. A goroutine pumps the upstream
	// and emits each chunk via host.stream.emit so the client sees true streaming.
	go pumpUpstreamStreamWithRetry(body, sa, req.StreamID, sseFramed)
	return okEnvelope(streamResponse{Headers: headers})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStreamWithRetry wraps pumpUpstreamStream with transparent 401 token refresh on first packet.
func pumpUpstreamStreamWithRetry(body []byte, sa *storedAuth, streamID string, sseFramed bool) {
	for attempt := 0; attempt < 2; attempt++ {
		httpReq, err := http.NewRequest(http.MethodPost, baseURLFor(sa)+pathChat, bytes.NewReader(body))
		if err != nil {
			streamEmitError(streamID, err.Error())
			streamClose(streamID)
			return
		}
		backendHeaders(httpReq, sa)

		resp, err := sharedHTTPClient().Do(httpReq)
		if err != nil {
			streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
			streamClose(streamID)
			return
		}

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			refreshMu.Lock()
			refErr := refreshAuthTokenLocked(sa)
			if refErr == nil {
				_ = saveStoredAuthAtomic(sa, authFileName)
			}
			refreshMu.Unlock()
			if refErr == nil {
				continue // retry request with fresh accessToken
			}
		}

		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			errPayload, _ := io.ReadAll(resp.Body)
			streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", resp.StatusCode, truncate(string(errPayload), 200)))
			streamClose(streamID)
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			content := stripDataPrefix(scanner.Text())
			if content == "" || content == "[DONE]" {
				continue
			}
			cleaned := cleanChunkJSON(content)
			if cleaned == "" {
				continue
			}
			if sseFramed {
				cleaned = "data: " + cleaned
			}
			if err := streamEmit(streamID, []byte(cleaned)); err != nil {
				// Client abort or stream closed by host -> exit scanner and close body immediately
				break
			}
		}
		streamClose(streamID)
		return
	}
}
// collectUpstreamStream is the synchronous fallback (no async stream id): drain
// the upstream, clean each chunk, return them as a slice.
func collectUpstreamStream(body []byte, sa *storedAuth, sseFramed bool) ([]pluginapi.ExecutorStreamChunk, error) {
	httpReq, err := http.NewRequest(http.MethodPost, baseURLFor(sa)+pathChat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	backendHeaders(httpReq, sa)
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http_error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errPayload, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(errPayload), 200))
	}
	return aggregateSSE(resp.Body, sseFramed), nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// aggregateSSE reads an upstream SSE stream and emits one chunk per data event.
// Empty-valued delta fields are stripped and the trailing [DONE] is dropped
// (the host appends its own stream terminator). When sseFramed is true each
// payload is emitted as a "data: " line for cross-format translators; otherwise
// the payload is the raw JSON object and the host chat-completions writer adds
// the framing itself.
func aggregateSSE(r io.Reader, sseFramed bool) []pluginapi.ExecutorStreamChunk {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var chunks []pluginapi.ExecutorStreamChunk
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		cleaned := cleanChunkJSON(content)
		if cleaned == "" {
			continue
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(cleaned)})
	}
	return chunks
}

// cleanChunkJSON strips empty-valued fields (null/""/[]/{}) from choice deltas
// so strict clients don't trip on {"function_call":null,"tool_calls":[]}.
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				for k, v := range delta {
					if isEmptyValue(v) {
						delete(delta, k)
					}
				}
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// forceStreamBody returns the request body with "stream":true set, since the
// upstream rejects non-streaming chat requests.
func forceStreamBody(payload, original []byte) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// rewriteSystemForUpstream neutralizes Claude Code template phrases that
// Tencent CodeBuddy's content filter blocklists verbatim — the agent identity
// line ("You are Claude Code, Anthropic's official CLI for Claude.") and the
// git injection ("Main branch (you will usually use this for PRs)"). Each
// rewrite is a single-word change so the prompt's meaning is preserved while
// dodging the exact-match filter.
func rewriteSystemForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	messages, _ := obj["messages"].([]any)
	changed := false

	// Upstream enforces that the first message must be a system prompt (code 11128)
	if len(messages) == 0 {
		messages = []any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}
		obj["messages"] = messages
		changed = true
	} else if first, ok := messages[0].(map[string]any); !ok || first["role"] != "system" {
		systemMsg := map[string]any{"role": "system", "content": "You are a helpful assistant."}
		messages = append([]any{systemMsg}, messages...)
		obj["messages"] = messages
		changed = true
	}

	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if rewriteContentField(msg) {
			changed = true
		}
	}
	if forceMaxThinking(obj) {
		changed = true
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteContentField sanitizes blocked templates in one message's content,
// handling both plain-string and OpenAI multimodal (array of parts) shapes.
// Returns true if the message was modified.
func rewriteContentField(msg map[string]any) bool {
	switch c := msg["content"].(type) {
	case string:
		if r := sanitizeBlockedTemplates(c); r != c {
			msg["content"] = r
			return true
		}
	case []any:
		modified := false
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok {
				if r := sanitizeBlockedTemplates(t); r != t {
					part["text"] = r
					modified = true
				}
			}
		}
		return modified
	}
	return false
}

func sanitizeBlockedTemplates(s string) string {
	// 1. Built-in base rules
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")

	// 2. Dynamic rules loaded from workbuddy.json if present
	for _, rule := range dynamicRules() {
		if rule.Pattern != "" {
			s = strings.ReplaceAll(s, rule.Pattern, rule.Replacement)
		}
	}
	return s
}

var (
	cachedRules   []rewriteRule
	cachedRulesMu sync.RWMutex
)

func dynamicRules() []rewriteRule {
	cachedRulesMu.RLock()
	rules := cachedRules
	cachedRulesMu.RUnlock()
	if rules != nil {
		return rules
	}

	cachedRulesMu.Lock()
	defer cachedRulesMu.Unlock()
	if cachedRules != nil {
		return cachedRules
	}

	data, err := os.ReadFile(authFileName)
	if err == nil {
		var sa storedAuth
		if json.Unmarshal(data, &sa) == nil && len(sa.RewriteRules) > 0 {
			cachedRules = sa.RewriteRules
			return cachedRules
		}
	}
	cachedRules = []rewriteRule{}
	return cachedRules
}

// setDynamicRules allows unit tests or runtime reload to inject rules
func setDynamicRules(rules []rewriteRule) {
	cachedRulesMu.Lock()
	cachedRules = rules
	cachedRulesMu.Unlock()
}

// forceMaxThinking pins reasoning_effort to "high" for hy3-family models and
// normalizes Anthropic-style thinking objects into upstream-compatible parameters.
func forceMaxThinking(obj map[string]any) bool {
	changed := false
	model, _ := obj["model"].(string)

	// If Anthropic-style thinking block is provided, normalize or translate it
	if thinking, ok := obj["thinking"].(map[string]any); ok {
		if tType, _ := thinking["type"].(string); tType == "enabled" {
			if _, exists := obj["reasoning_effort"]; !exists {
				obj["reasoning_effort"] = "high"
				changed = true
			}
		}
	}

	if strings.HasPrefix(model, "hy3") {
		if eff, _ := obj["reasoning_effort"].(string); eff != "high" {
			obj["reasoning_effort"] = "high"
			changed = true
		}
	}
	return changed
}

// aggregateCompletion folds an SSE stream into a single non-streaming
// chat.completion object (used for non-stream client requests).
func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var content, reasoning, role, respModel, respID, finish string
	var created int64
	var usage map[string]any
	// indexed aggregation for streaming tool_calls chunks
	type toolCallBuilder struct {
		id       string
		callType string
		name     string
		args     strings.Builder
	}
	toolMap := make(map[int]*toolCallBuilder)
	var toolOrder []int

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			respID = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok {
			created = int64(v)
		}
		if v, ok := chunk["usage"].(map[string]any); ok {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if idxF, ok := call["index"].(float64); ok {
							idx = int(idxF)
						}
						b, exists := toolMap[idx]
						if !exists {
							b = &toolCallBuilder{callType: "function"}
							toolMap[idx] = b
							toolOrder = append(toolOrder, idx)
						}
						if id, ok := call["id"].(string); ok && id != "" {
							b.id = id
						}
						if tp, ok := call["type"].(string); ok && tp != "" {
							b.callType = tp
						}
						if fn, ok := call["function"].(map[string]any); ok {
							if name, ok := fn["name"].(string); ok && name != "" {
								b.name = name
							}
							if args, ok := fn["arguments"].(string); ok {
								b.args.WriteString(args)
							}
						}
					}
				}
			}
			if v, ok := choice["finish_reason"].(string); ok && v != "" {
				finish = v
			}
		}
	}

	var aggregatedTools []map[string]any
	for _, idx := range toolOrder {
		b := toolMap[idx]
		aggregatedTools = append(aggregatedTools, map[string]any{
			"id":   firstNonEmpty(b.id, fmt.Sprintf("call_%d", idx)),
			"type": firstNonEmpty(b.callType, "function"),
			"function": map[string]any{
				"name":      b.name,
				"arguments": b.args.String(),
			},
		})
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(aggregatedTools) > 0 {
		message["tool_calls"] = aggregatedTools
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(respID, "chatcmpl-workbuddy"),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}
// normalizeUpstreamError converts upstream error payloads (e.g. 11101, 10003, 401, 429) into standard OpenAI error envelopes.
func normalizeUpstreamError(statusCode int, raw []byte) []byte {
	var env apiEnvelope
	_ = json.Unmarshal(raw, &env)

	msg := string(raw)
	codeStr := fmt.Sprintf("%d", statusCode)
	errType := "upstream_error"

	if env.Code != 0 || env.Msg != "" {
		msg = env.Msg
		codeStr = fmt.Sprintf("%d", env.Code)
	}

	switch env.Code {
	case 11101:
		msg = "upstream requires streaming requests (stream: true)"
		errType = "invalid_request_error"
	case 10003:
		msg = "invalid or expired token"
		errType = "authentication_error"
	}

	switch statusCode {
	case http.StatusUnauthorized:
		errType = "authentication_error"
	case http.StatusTooManyRequests:
		errType = "rate_limit_exceeded"
	}

	errObj := map[string]any{
		"error": map[string]any{
			"message": truncate(msg, 300),
			"type":    errType,
			"code":    codeStr,
		},
	}
	out, _ := json.Marshal(errObj)
	return out
}

// -----------------------------------------------------------------------------
// envelope helpers
// -----------------------------------------------------------------------------

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
