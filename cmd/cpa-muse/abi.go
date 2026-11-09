package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

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

extern int musePluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void musePluginFree(void*, size_t);
extern void musePluginShutdown(void);

static int muse_call_host(const cliproxy_host_api* host, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static void muse_free_host_buffer(const cliproxy_host_api* host, void* ptr, size_t len) {
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/sbga-tech/cpa-muse/internal/host"
	"github.com/sbga-tech/cpa-muse/internal/plugin"
)

var hostAPI = struct {
	sync.RWMutex
	api *C.cliproxy_host_api
}{}

//export cliproxy_plugin_init
func cliproxy_plugin_init(api *C.cliproxy_host_api, out *C.cliproxy_plugin_api) C.int {
	if api == nil || out == nil {
		return 1
	}
	if uint32(api.abi_version) != pluginabi.ABIVersion {
		return 1
	}
	hostAPI.Lock()
	hostAPI.api = api
	hostAPI.Unlock()
	host.SetTransport(callHost)
	out.abi_version = C.uint32_t(pluginabi.ABIVersion)
	out.call = C.cliproxy_plugin_call_fn(C.musePluginCall)
	out.free_buffer = C.cliproxy_plugin_free_fn(C.musePluginFree)
	out.shutdown = C.cliproxy_plugin_shutdown_fn(C.musePluginShutdown)
	return 0
}

//export musePluginCall
func musePluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeBuffer(response, plugin.ErrorEnvelope("invalid_method", "method is required", 0))
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeBuffer(response, plugin.Dispatch(C.GoString(method), payload))
	return 0
}

//export musePluginFree
func musePluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export musePluginShutdown
func musePluginShutdown() {
	plugin.Shutdown()
	host.SetTransport(nil)
	hostAPI.Lock()
	hostAPI.api = nil
	hostAPI.Unlock()
}

// callHost forwards one host callback and returns the raw envelope bytes.
func callHost(method string, request []byte) ([]byte, error) {
	hostAPI.RLock()
	api := hostAPI.api
	hostAPI.RUnlock()
	if api == nil {
		return nil, fmt.Errorf("host callback is unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPtr *C.uint8_t
	if len(request) > 0 {
		requestPtr = (*C.uint8_t)(C.CBytes(request))
		defer C.free(unsafe.Pointer(requestPtr))
	}
	var response C.cliproxy_buffer
	code := C.muse_call_host(api, cMethod, requestPtr, C.size_t(len(request)), &response)
	if response.ptr != nil {
		defer C.muse_free_host_buffer(api, response.ptr, response.len)
	}
	if code != 0 {
		return nil, fmt.Errorf("host callback %s failed with code %d", method, int(code))
	}
	if response.ptr == nil || response.len == 0 {
		return nil, nil
	}
	return C.GoBytes(response.ptr, C.int(response.len)), nil
}

func writeBuffer(response *C.cliproxy_buffer, data []byte) {
	if response == nil || len(data) == 0 {
		return
	}
	ptr := C.malloc(C.size_t(len(data)))
	if ptr == nil {
		return
	}
	C.memcpy(ptr, unsafe.Pointer(&data[0]), C.size_t(len(data)))
	response.ptr = ptr
	response.len = C.size_t(len(data))
}
