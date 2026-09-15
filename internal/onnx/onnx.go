// Package onnx is recall's own minimal CGO bridge to Microsoft's
// ONNX Runtime. We deliberately do not depend on any third-party Go
// binding: we vendor only the official MIT-licensed C header
// (onnxruntime_c_api.h) and call ~25 functions through it via dlopen,
// which keeps the audit surface to: (1) the official Microsoft library
// loaded at runtime (SHA256-pinned), (2) ~150 lines of our C wrapper
// (bridge.c), and (3) ~250 lines of Go (this file + session.go).
//
// The C wrapper layer (bridge.c / bridge.h) flattens ONNX Runtime's
// function-pointer table into ordinary C functions; the CGO surface here
// is one wrapper call per primitive operation. Memory ownership: every
// Create*/Get*Name returns a heap object that must be released; the
// session.go layer pairs each Create with a Destroy or wraps in a
// finalizer so callers don't have to track this manually.
package onnx

/*
#cgo darwin LDFLAGS: -ldl
#cgo linux LDFLAGS: -ldl
// Windows needs no extra link flags: LoadLibrary/GetProcAddress live in
// kernel32, linked by mingw by default. -ldl must NOT be passed there
// (there is no libdl on Windows) — hence the per-OS #cgo lines above.

#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// Logging levels mirror OrtLoggingLevel. Use Warning by default — Info
// floods logs, Verbose floods even more.
const (
	LogVerbose = 0
	LogInfo    = 1
	LogWarning = 2
	LogError   = 3
	LogFatal   = 4
)

// Graph optimisation levels mirror GraphOptimizationLevel.
const (
	GraphOptDisable = 0
	GraphOptBasic   = 1
	GraphOptExt     = 2
	GraphOptAll     = 99
)

// Element types mirror ONNXTensorElementDataType. We expose only the
// types recall uses; add more as needed.
type ElementType int

const (
	TypeFloat32 ElementType = 1  // ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT
	TypeInt64   ElementType = 7  // ONNX_TENSOR_ELEMENT_DATA_TYPE_INT64
	TypeFloat16 ElementType = 10 // ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT16
	TypeUint8   ElementType = 2  // for quantised tensors when needed
	TypeInt8    ElementType = 3
)

var (
	loadOnce sync.Once
	loadErr  error
)

// Load opens the ONNX Runtime dylib via dlopen and captures the OrtApi
// pointer. Idempotent — safe to call from many goroutines or many
// embedder constructions; the underlying load happens once.
func Load(dylibPath string) error {
	loadOnce.Do(func() {
		cpath := C.CString(dylibPath)
		defer C.free(unsafe.Pointer(cpath))
		var cerr *C.char
		rc := C.am_onnx_load(cpath, &cerr)
		if rc != 0 {
			msg := "unknown"
			if cerr != nil {
				msg = C.GoString(cerr)
			}
			loadErr = fmt.Errorf("load %s: rc=%d %s", dylibPath, rc, msg)
		}
	})
	return loadErr
}

// Version returns the ONNX Runtime build string (e.g. "1.25.1") from the
// dylib. Returns "" if Load hasn't been called or failed.
func Version() string {
	cs := C.am_onnx_version()
	if cs == nil {
		return ""
	}
	return C.GoString(cs)
}

// statusErr converts an *OrtStatus return to a Go error. Returns nil for
// a NULL status (success), otherwise reads the message and releases.
func statusErr(s *C.OrtStatus) error {
	if s == nil {
		return nil
	}
	defer C.am_release_status(s)
	cmsg := C.am_status_message(s)
	if cmsg == nil {
		return errors.New("ONNX runtime: unknown error")
	}
	return errors.New("ONNX runtime: " + C.GoString(cmsg))
}
