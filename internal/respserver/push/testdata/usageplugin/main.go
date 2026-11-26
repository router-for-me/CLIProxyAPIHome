// This test-only plugin advertises one capability and records actual usage.handle calls.
package main

/*
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct { const char *ptr; size_t len; } buffer;
typedef struct {
	uint32_t abi_version;
	int (*call)(const char *, const uint8_t *, size_t, buffer *);
	void (*free_buffer)(void *, size_t);
	void (*shutdown)(void);
} plugin_api;

#define REGISTER(cap) "{\"ok\":true,\"result\":{\"schema_version\":1,\"metadata\":{" \
	"\"Name\":\"probe\",\"Version\":\"1.0.0\",\"Author\":\"test\"," \
	"\"GitHubRepository\":\"https://github.com/router-for-me/CLIProxyAPIHome\"}," \
	"\"capabilities\":{\"" cap "\":true}}}"

static int call(const char *method, const uint8_t *req, size_t len, buffer *res) {
	const char *mode = getenv("CPA_HOME_TEST_PLUGIN_MODE");
	if (mode == NULL) return 1;
	if (!strcmp(method, "plugin.register") || !strcmp(method, "plugin.reconfigure")) {
		res->ptr = !strcmp(mode, "usage") ? REGISTER("usage_plugin") : REGISTER("scheduler");
	} else if (!strcmp(method, "usage.handle") && !strcmp(mode, "usage")) {
		FILE *file = fopen("observed.jsonl", "ab");
		if (file == NULL) return 1;
		int ok = fwrite(req, 1, len, file) == len && fputc('\n', file) != EOF;
		if (fclose(file) != 0 || !ok) return 1;
		res->ptr = "{\"ok\":true,\"result\":{}}";
	} else {
		return 1;
	}
	res->len = strlen(res->ptr);
	return 0;
}

// Responses point at static literals.
static void free_buffer(void *ptr, size_t len) {}
static void shutdown_plugin(void) {}
static void init_api(plugin_api *api) {
	api->abi_version = 1;
	api->call = call;
	api->free_buffer = free_buffer;
	api->shutdown = shutdown_plugin;
}
*/
import "C"

import "unsafe"

//export cliproxy_plugin_init
func cliproxy_plugin_init(host unsafe.Pointer, api *C.plugin_api) C.int {
	C.init_api(api)
	return 0
}

func main() {}
