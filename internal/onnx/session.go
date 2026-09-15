package onnx

/*
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

// Env is a process-wide ONNX runtime environment. One per Go process is
// usually sufficient. Telemetry is disabled by default.
type Env struct {
	c *C.OrtEnv
}

// NewEnv allocates an OrtEnv with the given log level and identifier.
// Telemetry is disabled (Microsoft's ONNX Runtime occasionally pings
// Microsoft endpoints if not explicitly disabled).
func NewEnv(level int, name string) (*Env, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	var c *C.OrtEnv
	if err := statusErr(C.am_create_env(C.int(level), cname, &c)); err != nil {
		return nil, err
	}
	if err := statusErr(C.am_disable_env_telemetry(c)); err != nil {
		C.am_release_env(c)
		return nil, fmt.Errorf("disable telemetry: %w", err)
	}
	e := &Env{c: c}
	runtime.SetFinalizer(e, (*Env).Close)
	return e, nil
}

func (e *Env) Close() {
	if e == nil || e.c == nil {
		return
	}
	C.am_release_env(e.c)
	e.c = nil
	runtime.SetFinalizer(e, nil)
}

// SessionOptions controls how a session is built. Defaults match
// "best effort on this hardware": all graph optimisations on, default
// thread counts.
type SessionOptions struct {
	c *C.OrtSessionOptions
}

func NewSessionOptions() (*SessionOptions, error) {
	var c *C.OrtSessionOptions
	if err := statusErr(C.am_create_session_options(&c)); err != nil {
		return nil, err
	}
	o := &SessionOptions{c: c}
	runtime.SetFinalizer(o, (*SessionOptions).Close)
	return o, nil
}

func (o *SessionOptions) SetIntraOpThreads(n int) error {
	return statusErr(C.am_session_options_set_intra_op_threads(o.c, C.int(n)))
}
func (o *SessionOptions) SetInterOpThreads(n int) error {
	return statusErr(C.am_session_options_set_inter_op_threads(o.c, C.int(n)))
}
func (o *SessionOptions) SetGraphOptLevel(level int) error {
	return statusErr(C.am_session_options_set_graph_opt_level(o.c, C.int(level)))
}

// EnableCoreML appends the Apple Neural Engine / CoreML execution
// provider. Returns an error wrapping "not implemented" if the loaded
// libonnxruntime wasn't compiled with CoreML support.
func (o *SessionOptions) EnableCoreML(flags uint32) error {
	return statusErr(C.am_session_options_append_coreml(o.c, C.uint(flags)))
}

func (o *SessionOptions) Close() {
	if o == nil || o.c == nil {
		return
	}
	C.am_release_session_options(o.c)
	o.c = nil
	runtime.SetFinalizer(o, nil)
}

// Session wraps a loaded ONNX model.
type Session struct {
	c *C.OrtSession
	// Held by reference so the env / opts aren't GC'd while the session is alive.
	env  *Env
	opts *SessionOptions
}

// NewSession loads modelPath into a runnable session.
func NewSession(env *Env, modelPath string, opts *SessionOptions) (*Session, error) {
	if env == nil || env.c == nil {
		return nil, fmt.Errorf("nil env")
	}
	if opts == nil {
		return nil, fmt.Errorf("nil session options")
	}
	cpath := C.CString(modelPath)
	defer C.free(unsafe.Pointer(cpath))
	var c *C.OrtSession
	if err := statusErr(C.am_create_session(env.c, cpath, opts.c, &c)); err != nil {
		return nil, err
	}
	s := &Session{c: c, env: env, opts: opts}
	runtime.SetFinalizer(s, (*Session).Close)
	return s, nil
}

func (s *Session) Close() {
	if s == nil || s.c == nil {
		return
	}
	C.am_release_session(s.c)
	s.c = nil
	runtime.SetFinalizer(s, nil)
}

// InputNames returns the model's input tensor names in declaration order.
func (s *Session) InputNames() ([]string, error) {
	var n C.size_t
	if err := statusErr(C.am_session_input_count(s.c, &n)); err != nil {
		return nil, err
	}
	out := make([]string, int(n))
	for i := 0; i < int(n); i++ {
		var cs *C.char
		if err := statusErr(C.am_session_input_name(s.c, C.size_t(i), &cs)); err != nil {
			return nil, err
		}
		out[i] = C.GoString(cs)
		C.am_free_alloc(unsafe.Pointer(cs))
	}
	return out, nil
}

// OutputNames returns the model's output tensor names in declaration order.
func (s *Session) OutputNames() ([]string, error) {
	var n C.size_t
	if err := statusErr(C.am_session_output_count(s.c, &n)); err != nil {
		return nil, err
	}
	out := make([]string, int(n))
	for i := 0; i < int(n); i++ {
		var cs *C.char
		if err := statusErr(C.am_session_output_name(s.c, C.size_t(i), &cs)); err != nil {
			return nil, err
		}
		out[i] = C.GoString(cs)
		C.am_free_alloc(unsafe.Pointer(cs))
	}
	return out, nil
}

// Run feeds inputs (paired with inputNames in order) and writes the
// outputs to the pre-allocated output tensors (paired with outputNames).
// All slices must have matching lengths.
func (s *Session) Run(inputNames []string, inputs []*Tensor, outputNames []string, outputs []*Tensor) error {
	if len(inputNames) != len(inputs) {
		return fmt.Errorf("inputs: names=%d tensors=%d", len(inputNames), len(inputs))
	}
	if len(outputNames) != len(outputs) {
		return fmt.Errorf("outputs: names=%d tensors=%d", len(outputNames), len(outputs))
	}

	cInputNames := make([]*C.char, len(inputNames))
	for i, n := range inputNames {
		cInputNames[i] = C.CString(n)
		defer C.free(unsafe.Pointer(cInputNames[i]))
	}
	cInputs := make([]*C.OrtValue, len(inputs))
	for i, t := range inputs {
		cInputs[i] = t.c
	}

	cOutputNames := make([]*C.char, len(outputNames))
	for i, n := range outputNames {
		cOutputNames[i] = C.CString(n)
		defer C.free(unsafe.Pointer(cOutputNames[i]))
	}
	cOutputs := make([]*C.OrtValue, len(outputs))
	for i, t := range outputs {
		cOutputs[i] = t.c
	}

	var inNamesPtr **C.char
	if len(cInputNames) > 0 {
		inNamesPtr = (**C.char)(unsafe.Pointer(&cInputNames[0]))
	}
	var inPtr **C.OrtValue
	if len(cInputs) > 0 {
		inPtr = (**C.OrtValue)(unsafe.Pointer(&cInputs[0]))
	}
	var outNamesPtr **C.char
	if len(cOutputNames) > 0 {
		outNamesPtr = (**C.char)(unsafe.Pointer(&cOutputNames[0]))
	}
	var outPtr **C.OrtValue
	if len(cOutputs) > 0 {
		outPtr = (**C.OrtValue)(unsafe.Pointer(&cOutputs[0]))
	}

	return statusErr(C.am_session_run(
		s.c, nil,
		(**C.char)(unsafe.Pointer(inNamesPtr)), (**C.OrtValue)(unsafe.Pointer(inPtr)),
		C.size_t(len(inputs)),
		(**C.char)(unsafe.Pointer(outNamesPtr)), (**C.OrtValue)(unsafe.Pointer(outPtr)),
		C.size_t(len(outputs)),
	))
}

// MemoryInfo describes where a tensor lives. We only use CPU.
type MemoryInfo struct {
	c *C.OrtMemoryInfo
}

func NewCPUMemoryInfo() (*MemoryInfo, error) {
	var c *C.OrtMemoryInfo
	if err := statusErr(C.am_create_cpu_memory_info(&c)); err != nil {
		return nil, err
	}
	m := &MemoryInfo{c: c}
	runtime.SetFinalizer(m, (*MemoryInfo).Close)
	return m, nil
}
func (m *MemoryInfo) Close() {
	if m == nil || m.c == nil {
		return
	}
	C.am_release_memory_info(m.c)
	m.c = nil
	runtime.SetFinalizer(m, nil)
}

// Tensor is an OrtValue wrapper. The underlying buffer is allocated by
// ORT; Tensor owns it for the duration of the wrapper's lifetime.
type Tensor struct {
	c     *C.OrtValue
	shape []int64
	etype ElementType
	owned bool // true if backing data is allocator-owned (free via ReleaseValue alone)
}

// NewEmptyTensor allocates an output tensor with the given shape +
// element type. Used for outputs that ORT will fill during Run.
func NewEmptyTensor(mi *MemoryInfo, shape []int64, t ElementType) (*Tensor, error) {
	var c *C.OrtValue
	var shapePtr *C.int64_t
	if len(shape) > 0 {
		shapePtr = (*C.int64_t)(unsafe.Pointer(&shape[0]))
	}
	if err := statusErr(C.am_create_tensor_alloc(
		mi.c, shapePtr, C.size_t(len(shape)),
		C.ONNXTensorElementDataType(t), &c,
	)); err != nil {
		return nil, err
	}
	tn := &Tensor{c: c, shape: append([]int64(nil), shape...), etype: t, owned: true}
	runtime.SetFinalizer(tn, (*Tensor).Close)
	return tn, nil
}

// NewInt64Tensor wraps a Go []int64 as a tensor view. The backing slice
// must outlive the tensor (Run reads from it; we hold a reference).
func NewInt64Tensor(mi *MemoryInfo, shape []int64, data []int64) (*Tensor, error) {
	if prod(shape) != int64(len(data)) {
		return nil, fmt.Errorf("shape %v requires %d elems, got %d", shape, prod(shape), len(data))
	}
	var c *C.OrtValue
	var shapePtr *C.int64_t
	if len(shape) > 0 {
		shapePtr = (*C.int64_t)(unsafe.Pointer(&shape[0]))
	}
	if err := statusErr(C.am_create_tensor_with_data(
		mi.c, unsafe.Pointer(&data[0]), C.size_t(len(data)*8),
		shapePtr, C.size_t(len(shape)),
		C.ONNXTensorElementDataType(TypeInt64), &c,
	)); err != nil {
		return nil, err
	}
	tn := &Tensor{c: c, shape: append([]int64(nil), shape...), etype: TypeInt64}
	runtime.SetFinalizer(tn, (*Tensor).Close)
	return tn, nil
}

// NewFloat32Tensor is the float32 equivalent of NewInt64Tensor.
func NewFloat32Tensor(mi *MemoryInfo, shape []int64, data []float32) (*Tensor, error) {
	if prod(shape) != int64(len(data)) {
		return nil, fmt.Errorf("shape %v requires %d elems, got %d", shape, prod(shape), len(data))
	}
	var c *C.OrtValue
	var shapePtr *C.int64_t
	if len(shape) > 0 {
		shapePtr = (*C.int64_t)(unsafe.Pointer(&shape[0]))
	}
	if err := statusErr(C.am_create_tensor_with_data(
		mi.c, unsafe.Pointer(&data[0]), C.size_t(len(data)*4),
		shapePtr, C.size_t(len(shape)),
		C.ONNXTensorElementDataType(TypeFloat32), &c,
	)); err != nil {
		return nil, err
	}
	tn := &Tensor{c: c, shape: append([]int64(nil), shape...), etype: TypeFloat32}
	runtime.SetFinalizer(tn, (*Tensor).Close)
	return tn, nil
}

// Shape returns the tensor's static shape (as recorded at creation).
func (t *Tensor) Shape() []int64    { return append([]int64(nil), t.shape...) }
func (t *Tensor) Type() ElementType { return t.etype }

// Float32Data returns a slice into the tensor's backing buffer for
// float32 tensors. Caller must NOT use it after t is closed/finalised.
func (t *Tensor) Float32Data() ([]float32, error) {
	if t.etype != TypeFloat32 {
		return nil, fmt.Errorf("not a float32 tensor (type=%d)", t.etype)
	}
	var p unsafe.Pointer
	if err := statusErr(C.am_tensor_get_data(t.c, &p)); err != nil {
		return nil, err
	}
	n := prod(t.shape)
	return unsafe.Slice((*float32)(p), n), nil
}

// Int64Data returns a slice view for int64 tensors.
func (t *Tensor) Int64Data() ([]int64, error) {
	if t.etype != TypeInt64 {
		return nil, fmt.Errorf("not an int64 tensor (type=%d)", t.etype)
	}
	var p unsafe.Pointer
	if err := statusErr(C.am_tensor_get_data(t.c, &p)); err != nil {
		return nil, err
	}
	n := prod(t.shape)
	return unsafe.Slice((*int64)(p), n), nil
}

func (t *Tensor) Close() {
	if t == nil || t.c == nil {
		return
	}
	C.am_release_value(t.c)
	t.c = nil
	runtime.SetFinalizer(t, nil)
}

func prod(s []int64) int64 {
	if len(s) == 0 {
		return 0
	}
	p := int64(1)
	for _, d := range s {
		p *= d
	}
	return p
}
