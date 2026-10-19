package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct {void* ptr; size_t len;} cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*,const char*,const uint8_t*,size_t,cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*,size_t);
typedef struct {uint32_t abi_version;void* host_ctx;cliproxy_host_call_fn call;cliproxy_host_free_fn free_buffer;} cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*,uint8_t*,size_t,cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*,size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {uint32_t abi_version;cliproxy_plugin_call_fn call;cliproxy_plugin_free_fn free_buffer;cliproxy_plugin_shutdown_fn shutdown;} cliproxy_plugin_api;
extern int cliproxyPluginCall(char*,uint8_t*,size_t,cliproxy_buffer*);
extern void cliproxyPluginFree(void*,size_t);
extern void cliproxyPluginShutdown(void);
static cliproxy_host_api host;
static void set_host(cliproxy_host_api* h){host=*h;}
static int invoke_host(const char* method,const uint8_t* req,size_t n,cliproxy_buffer* out){if(!host.call)return 1;return host.call(host.host_ctx,method,req,n,out);}
static void release_host(cliproxy_buffer* b){if(b->ptr&&host.free_buffer)host.free_buffer(b->ptr,b->len);}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"unsafe"
)

func main() {
	if len(os.Args) > 1 {
		os.Exit(helper(os.Args[1:]))
	}
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(h *C.cliproxy_host_api, p *C.cliproxy_plugin_api) C.int {
	if h == nil || p == nil || h.abi_version != 1 {
		return 1
	}
	C.set_host(h)
	p.abi_version = 1
	p.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	p.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	p.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	hostCall = callHost
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, req *C.uint8_t, n C.size_t, out *C.cliproxy_buffer) (ret C.int) {
	if out == nil {
		return 1
	}
	out.ptr = nil
	out.len = 0
	defer func() {
		if recover() != nil {
			stopOnPanic()
			raw, _ := okEnvelope(denial("callback panic"))
			writeResponse(out, raw)
			ret = 0
		}
	}()
	if method == nil {
		return 1
	}
	if n > 16<<20 {
		return 1
	}
	var b []byte
	if n > 0 && req != nil {
		b = C.GoBytes(unsafe.Pointer(req), C.int(n))
	}
	raw, err := handleMethod(C.GoString(method), b)
	if err != nil {
		raw = errorEnvelope("plugin_error", "operation failed", 0)
		ret = 1
	}
	writeResponse(out, raw)
	return
}

//export cliproxyPluginFree
func cliproxyPluginFree(p unsafe.Pointer, _ C.size_t) {
	if p != nil {
		C.free(p)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { quiesce() }

func callHost(method string, payload, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	m := C.CString(method)
	defer C.free(unsafe.Pointer(m))
	var ptr unsafe.Pointer
	if len(b) > 0 {
		ptr = C.CBytes(b)
		defer C.free(ptr)
	}
	var response C.cliproxy_buffer
	code := C.invoke_host(m, (*C.uint8_t)(ptr), C.size_t(len(b)), &response)
	defer C.release_host(&response)
	if code != 0 || response.ptr == nil || response.len > 16<<20 {
		return fmt.Errorf("host callback unavailable")
	}
	var env envelope
	if json.Unmarshal(C.GoBytes(response.ptr, C.int(response.len)), &env) != nil || !env.OK {
		return fmt.Errorf("host callback rejected")
	}
	return json.Unmarshal(env.Result, out)
}
func writeResponse(out *C.cliproxy_buffer, b []byte) {
	if len(b) > 0 {
		out.ptr = C.CBytes(b)
		out.len = C.size_t(len(b))
	}
}
func okEnvelope(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return json.Marshal(envelope{OK: true, Result: b})
}
func errorEnvelope(c, m string, s int) []byte {
	b, _ := json.Marshal(envelope{Error: &envelopeError{Code: c, Message: m, HTTPStatus: s}})
	return b
}
func decodeJSON(b []byte, v any) error { return json.Unmarshal(b, v) }
