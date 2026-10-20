import re, sys
from pathlib import Path

# main.go 位于本脚本的上一级目录
p = Path(__file__).resolve().parent.parent / "main.go"
src = open(p).read()
orig = src

# 1. header doc
new_header = '''// Package main implements the WorkBuddy AI CLIProxyAPI dynamic plugin.
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
'''
src = re.sub(r"\A.*?\npackage main\n", new_header + "\npackage main\n", src, count=1, flags=re.S)

# 2. const block
old_const = '''	providerName  = "workbuddy"
	authFileName  = "workbuddy.json"
	upstreamBase  = "https://copilot.tencent.com"
	clientUA      = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer = "https://www.codebuddy.cn"

	endpointAuthState    = upstreamBase + "/v2/plugin/auth/state?platform=CLI"
	endpointLoginAcct    = upstreamBase + "/v2/plugin/login/account?state="
	endpointAuthToken    = upstreamBase + "/v2/plugin/auth/token?state="
	endpointTokenRefresh = upstreamBase + "/v2/plugin/auth/token/refresh"
	endpointChat         = upstreamBase + "/v2/chat/completions"
'''
new_const = '''	providerName  = "workbuddy"
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
'''
assert old_const in src
src = src.replace(old_const, new_const, 1)

# 3. loginCtx: add base
src = src.replace('''type loginCtx struct {
	client  *http.Client
	expires time.Time
}''', '''type loginCtx struct {
	client  *http.Client
	expires time.Time
	base    string
}''', 1)

# 4. realm helpers (insert before sharedHTTPClient)
helpers = '''
// baseURLFor resolves the upstream base URL for a stored credential:
// explicit baseURL wins, then the login-reported account domain, then the
// WorkBuddy AI.app international default.
func baseURLFor(sa *storedAuth) string {
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
'''
src = src.replace('''// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------
''', helpers, 1)

# 5. parseStored normalization
src = src.replace('''func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}''', '''func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	raw = normalizeStoredShape(raw)''', 1)

# 6. storedAuth: BaseURL field
src = src.replace('''type storedAuth struct {
	Auth    storedTokens  `json:"auth"`
	Account storedAccount `json:"account"`
}''', '''type storedAuth struct {
	Auth    storedTokens  `json:"auth"`
	Account storedAccount `json:"account"`
	BaseURL string        `json:"baseURL,omitempty"`
}''', 1)

# 7. login start: base-aware
src = src.replace('''func handleStartLogin(raw []byte) ([]byte, error) {
	client := newLoginClient()
	data, _, err := doJSON(client, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")))''',
'''func handleStartLogin(raw []byte) ([]byte, error) {
	base := loginBaseFromRaw(raw)
	client := newLoginClient()
	data, _, err := doJSON(client, http.MethodPost, base+pathAuthState, nil, bytes.NewReader([]byte("{}")))''', 1)
src = src.replace('loginStates.Store(st.State, &loginCtx{client: client, expires: time.Now().Add(loginTTL)})',
                  'loginStates.Store(st.State, &loginCtx{client: client, expires: time.Now().Add(loginTTL), base: base})', 1)

# 8. poll: lc.base endpoints
src = src.replace('endpointAuthToken+state', 'lc.base+pathAuthToken+state', 1)
src = src.replace('endpointLoginAcct+state', 'lc.base+pathLoginAcct+state', 1)

# 9. refresh + chat endpoints per realm
src = src.replace('doJSON(sharedHTTPClient(), http.MethodPost, endpointTokenRefresh, headers, nil)',
                  'doJSON(sharedHTTPClient(), http.MethodPost, baseURLFor(sa)+pathTokenRefresh, headers, nil)', 1)
n = src.count('http.NewRequest(http.MethodPost, endpointChat, bytes.NewReader(body))')
assert n == 3, n
src = src.replace('http.NewRequest(http.MethodPost, endpointChat, bytes.NewReader(body))',
                  'http.NewRequest(http.MethodPost, baseURLFor(sa)+pathChat, bytes.NewReader(body))')

# 10. commonHeaders origin from request URL
src = src.replace('''	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")''', '''	origin := originForHost(req.URL.Host)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")''', 1)

# 11. registration metadata
src = src.replace('''			Version:          "0.1.0",
			Author:           "lovingfish (clean-room rebuild; original workbuddy by Sliverkiss)",
			GitHubRepository: "https://github.com/lovingfish/workbuddy-cliproxy",''',
'''			Version:          pluginVersion,
			Author:           "workbuddy-CPA (based on workbuddy-cliproxy by lovingfish; original workbuddy by Sliverkiss)",
			GitHubRepository: "",''', 1)

# 12. pluginVersion var for ldflags (insert before func main() {})
src = src.replace('func main() {}', '''// pluginVersion is injected at link time for release builds:
//
//	-ldflags "-X main.pluginVersion=0.2.0"
var pluginVersion = "0.2.0"

func main() {}''', 1)

# sanity: no stale identifiers
for bad in ("upstreamBase", "originReferer", "endpointAuthState", "endpointLoginAcct",
            "endpointAuthToken", "endpointTokenRefresh", "endpointChat"):
    assert bad not in src, bad

assert src != orig
open(p, "w").write(src)
print("patched OK")
