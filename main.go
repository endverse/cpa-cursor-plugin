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

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) { stored_host = host; }

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"unsafe"

	"cpa-cursor/internal/plugin"
)

const abiVersion uint32 = 1

var runtimePlugin = plugin.New()

func init() {
	runtimePlugin.SetHost(func(method string, payload any) ([]byte, error) {
		return hostCall(method, payload)
	})
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, out *C.cliproxy_plugin_api) C.int {
	if out == nil {
		return 1
	}
	C.store_host_api(host)
	out.abi_version = C.uint32_t(abiVersion)
	out.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	out.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	out.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 400))
		return 1
	}
	var req []byte
	if request != nil && requestLen > 0 {
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	name := C.GoString(method)
	if name == "executor.execute_stream" {
		raw, rc := startStream(req)
		writeResponse(response, raw)
		return rc
	}
	result, err := runtimePlugin.Call(context.Background(), name, req)
	if err != nil {
		f := plugin.AsFail(err)
		writeResponse(response, errorEnvelope(f.Code, f.Msg, f.Status))
		return 1
	}
	body, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		writeResponse(response, errorEnvelope("plugin_error", errMarshal.Error(), 500))
		return 1
	}
	raw, _ := json.Marshal(envelope{OK: true, Result: body})
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, n C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = n
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func startStream(req []byte) ([]byte, C.int) {
	headers, chunks, err := runtimePlugin.OpenStream(context.Background(), req)
	if err != nil {
		f := plugin.AsFail(err)
		return errorEnvelope(f.Code, f.Msg, f.Status), 1
	}
	var in struct {
		StreamID string `json:"stream_id"`
	}
	_ = json.Unmarshal(req, &in)
	go func() {
		var failed error
		for chunk := range chunks {
			if _, errEmit := hostCall("host.stream.emit", map[string]any{"stream_id": in.StreamID, "payload": chunk}); errEmit != nil {
				failed = errEmit
				break
			}
		}
		closeBody := map[string]any{"stream_id": in.StreamID}
		if failed != nil {
			closeBody["error"] = failed.Error()
		}
		_, _ = hostCall("host.stream.close", closeBody)
	}()
	body, _ := json.Marshal(map[string]any{"headers": headers})
	raw, _ := json.Marshal(envelope{OK: true, Result: body})
	return raw, 0
}

func errorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
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

func hostCall(method string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(raw) > 0 {
		req = (*C.uint8_t)(C.CBytes(raw))
		defer C.free(unsafe.Pointer(req))
	}
	var response C.cliproxy_buffer
	rc := C.call_host_api(cMethod, req, C.size_t(len(raw)), &response)
	var body []byte
	if response.ptr != nil && response.len > 0 {
		body = C.GoBytes(response.ptr, C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 {
		return body, errHost
	}
	return body, nil
}

var errHost = hostError("host call failed")

type hostError string

func (e hostError) Error() string { return string(e) }
