/* host_smoke.c — real-consumer smoke for the cliproxy plugin C ABI.
 * dlopen the built plugin, feed it a fake host API, drive the exported
 * entry points and assert the envelope shapes. Usage: host_smoke <plugin>
 */
#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
    void *ptr;
    size_t len;
} cliproxy_buffer;

typedef struct {
    uint32_t abi_version;
    void *ctx;
    int (*call)(void *ctx, const char *method, const uint8_t *req, size_t req_len, cliproxy_buffer *out);
} cliproxy_host_api;

typedef struct {
    uint32_t abi_version;
    int (*call)(const char *method, const uint8_t *req, size_t req_len, cliproxy_buffer *out);
    void (*free_buffer)(void *ptr, size_t len);
    void (*shutdown)(void);
} cliproxy_plugin_api;

typedef int (*init_fn)(cliproxy_host_api *, cliproxy_plugin_api *);

static int fake_host_call(void *ctx, const char *method, const uint8_t *req, size_t req_len, cliproxy_buffer *out) {
    (void)ctx; (void)method; (void)req; (void)req_len;
    if (out) { out->ptr = NULL; out->len = 0; }
    return 0;
}

static int failures = 0;

static void expect_contains(const char *what, cliproxy_buffer *buf, const char *needle, int want) {
    if (!buf->ptr) { printf("FAIL %s: empty response\n", what); failures++; return; }
    char *s = (char *)malloc(buf->len + 1);
    memcpy(s, buf->ptr, buf->len);
    s[buf->len] = 0;
    int has = strstr(s, needle) != NULL;
    printf("%s %s: %s\n", (has == want) ? "PASS" : "FAIL", what, has ? "contains" : "missing");
    if (has != want) {
        size_t n = buf->len < 300 ? buf->len : 300;
        printf("  payload[%zu]: %.*s\n", buf->len, (int)n, s);
        failures++;
    }
    free(s);
}

int main(int argc, char **argv) {
    if (argc < 2) { fprintf(stderr, "usage: host_smoke <plugin>\n"); return 2; }
    void *h = dlopen(argv[1], RTLD_NOW);
    if (!h) { fprintf(stderr, "dlopen failed: %s\n", dlerror()); return 2; }
    init_fn init = (init_fn)dlsym(h, "cliproxy_plugin_init");
    if (!init) { fprintf(stderr, "missing cliproxy_plugin_init: %s\n", dlerror()); return 2; }
    cliproxy_host_api host = { 1u, NULL, fake_host_call };
    cliproxy_plugin_api plugin;
    memset(&plugin, 0, sizeof(plugin));
    if (init(&host, &plugin) != 0 || plugin.call == NULL) { printf("FAIL init\n"); return 1; }
    printf("PASS init abi=%u\n", plugin.abi_version);

    cliproxy_buffer out;
    plugin.call("plugin.register", NULL, 0, &out);
    expect_contains("plugin.register", &out, "\"Name\":\"workbuddy\"", 1);
    if (plugin.free_buffer && out.ptr) plugin.free_buffer(out.ptr, out.len);

    plugin.call("model.static", NULL, 0, &out);
    expect_contains("model.static", &out, "\"Provider\":\"workbuddy\"", 1);
    if (plugin.free_buffer && out.ptr) plugin.free_buffer(out.ptr, out.len);

    const char *bad = "{\"auth_json\":\"not json at all\"}";
    plugin.call("auth.parse", (const uint8_t *)bad, strlen(bad), &out);
    expect_contains("auth.parse invalid", &out, "\"Handled\":false", 1);
    if (plugin.free_buffer && out.ptr) plugin.free_buffer(out.ptr, out.len);

    plugin.call("no.such.method", NULL, 0, &out);
    expect_contains("unknown method", &out, "unknown_method", 1);
    if (plugin.free_buffer && out.ptr) plugin.free_buffer(out.ptr, out.len);

    if (plugin.shutdown) plugin.shutdown();
    dlclose(h);
    printf(failures ? "SMOKE FAIL (%d)\n" : "SMOKE PASS\n", failures);
    return failures ? 1 : 0;
}
