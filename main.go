// Command cliproxyapi-observer builds the CLIProxyAPI native observer plugin.
// This file is CGO glue only: every RPC method is forwarded into
// internal/plugin, which performs all observation and management work.
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

typedef int (*cliproxy_plugin_call_fn)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

// The host API is copied by value at init. These helpers only receive that
// value; no C global or borrowed pointer to the host-owned struct is retained.
static int observer_host_api_ready(cliproxy_host_api api) {
	return api.call != NULL && api.free_buffer != NULL;
}

static int observer_call_auth_list(cliproxy_host_api api, cliproxy_buffer* response) {
	if (response != NULL) {
		response->ptr = NULL;
		response->len = 0;
	}
	if (api.call == NULL) {
		return 1;
	}
	return api.call(api.host_ctx, "host.auth.list", NULL, 0, response);
}

static void observer_free_host_buffer(cliproxy_host_api api, void* ptr, size_t len) {
	if (ptr != NULL && api.free_buffer != NULL) {
		api.free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/fengwk/cliproxyapi-observer/internal/plugin"
	"github.com/fengwk/cliproxyapi-observer/internal/web"
)

// manager is shared by all native entry points.
var manager = plugin.NewManager(web.Asset)

// maxBufferLen guards the size_t to C.int conversion in C.GoBytes; requests at
// or above 2^31 would truncate to a negative length and panic outside recover.
const maxBufferLen = 1<<31 - 1

// maxHostAuthListBytes bounds the read-only metadata envelope copied out of the
// host ABI. Oversized responses are released without copying.
const maxHostAuthListBytes = 4 << 20

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	if uint32(host.abi_version) != pluginabi.ABIVersion {
		return 1
	}
	apiCopy := *host
	if C.observer_host_api_ready(apiCopy) != 0 {
		manager.SetAccountMetadataLookup(func(callbackID string) []byte {
			return lookupHostAuthList(apiCopy, callbackID)
		})
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			writeResponse(response, errorEnvelope("plugin_error", "internal panic"))
			rc = 1
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	if request == nil && requestLen != 0 {
		writeResponse(response, errorEnvelope("invalid_request", "missing request buffer"))
		return 0
	}
	var req []byte
	if request != nil && requestLen > 0 {
		if uint64(requestLen) > maxBufferLen {
			writeResponse(response, errorEnvelope("plugin_error", "request exceeds maximum size"))
			return 0
		}
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := manager.HandleCall(C.GoString(method), req)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", "internal error"))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() {
		_ = recover()
	}()
	_, _ = manager.HandleCall(pluginabi.MethodPluginShutdown, nil)
}

// lookupHostAuthList performs the only host callback this plugin uses for
// display names: a read-only metadata list. It never reads credential JSON or
// calls host.auth.get/save. Every returned buffer is released through the same
// copied ABI value that produced it, including error and oversized paths.
func lookupHostAuthList(api C.cliproxy_host_api, callbackID string) (out []byte) {
	if callbackID == "" {
		return nil
	}
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	var response C.cliproxy_buffer
	rc := C.observer_call_auth_list(api, &response)
	if response.ptr != nil {
		defer C.observer_free_host_buffer(api, response.ptr, response.len)
	}
	if rc != 0 || response.ptr == nil || response.len == 0 {
		return nil
	}
	// Check before C.GoBytes: size_t at or above 2^31 would truncate to a
	// negative C.int and panic outside the native entry point's recover.
	if uint64(response.len) > maxHostAuthListBytes || uint64(response.len) > maxBufferLen {
		return nil
	}
	return C.GoBytes(response.ptr, C.int(response.len))
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

func errorEnvelope(code, message string) []byte {
	type envelopeError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	raw, err := json.Marshal(struct {
		OK    bool           `json:"ok"`
		Error *envelopeError `json:"error"`
	}{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"internal error"}}`)
	}
	return raw
}

func main() {}
