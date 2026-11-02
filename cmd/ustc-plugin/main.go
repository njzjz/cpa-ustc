// ustc-plugin builds a standard C shared library, not a Go plugin package.
package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void *ptr; size_t len; } cliproxy_buffer;
typedef int (*host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void *host_ctx; host_call_fn call; free_fn free_buffer; } cliproxy_host_api;
typedef int (*plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*shutdown_fn)(void);
typedef struct { uint32_t abi_version; plugin_call_fn call; free_fn free_buffer; shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
static cliproxy_host_api saved_host;
static void save_host(const cliproxy_host_api *host) { saved_host = *host; }
static int host_call(const char *method, const uint8_t *data, size_t size, cliproxy_buffer *result) {
    if (!saved_host.call) return 1;
    return saved_host.call(saved_host.host_ctx, method, data, size, result);
}
static void host_free(void *data, size_t size) { if (data && saved_host.free_buffer) saved_host.free_buffer(data, size); }
*/
import "C"

import (
	"encoding/json"
	"errors"
	"unsafe"

	"cpa-ustc/internal/plugin"
)

var runtime = plugin.New(callHost)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || host.abi_version != 1 || host.call == nil || host.free_buffer == nil {
		return 1
	}
	C.save_host(host)
	api.abi_version = 1
	api.call = C.plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.free_fn(C.cliproxyPluginFree)
	api.shutdown = C.shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) (status C.int) {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeResponse(response, []byte(`{"ok":false,"error":{"code":"panic","message":"USTC plugin internal error"}}`))
			status = 1
		}
	}()
	if method == nil || length > 64<<20 || (length > 0 && request == nil) {
		writeResponse(response, []byte(`{"ok":false,"error":{"code":"invalid_request","message":"invalid ABI arguments"}}`))
		return 1
	}
	data := C.GoBytes(unsafe.Pointer(request), C.int(length))
	writeResponse(response, runtime.Dispatch(C.GoString(method), data))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(pointer unsafe.Pointer, _ C.size_t) { C.free(pointer) }

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { runtime.Close() }

func writeResponse(response *C.cliproxy_buffer, data []byte) {
	response.ptr = C.CBytes(data)
	response.len = C.size_t(len(data))
}

func callHost(method string, request any, result any) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	name := C.CString(method)
	defer C.free(unsafe.Pointer(name))
	payload := C.CBytes(data)
	defer C.free(payload)
	var response C.cliproxy_buffer
	status := C.host_call(name, (*C.uint8_t)(payload), C.size_t(len(data)), &response)
	if response.ptr == nil {
		return errors.New("host callback returned no response")
	}
	defer C.host_free(response.ptr, response.len)
	if response.len > 64<<20 {
		return errors.New("host callback response too large")
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if status != 0 || json.Unmarshal(C.GoBytes(response.ptr, C.int(response.len)), &envelope) != nil || !envelope.OK {
		return errors.New("host callback failed: " + method)
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}
