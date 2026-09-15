/* See bridge.h for design. All functions delegate through the global
 * OrtApi pointer captured at am_onnx_load time. */

#include "bridge.h"
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Cross-platform dynamic loading. POSIX provides dlopen/dlsym/dlerror via
 * <dlfcn.h>. Windows has no dlfcn, so we shim those three onto
 * LoadLibraryA / GetProcAddress / FormatMessage. The call sites below
 * (am_onnx_load + the dlsym lookups) are then identical on every OS. */
#ifdef _WIN32
#include <windows.h>
#define RTLD_LAZY 0
static void *am_dlopen(const char *path, int flag) {
    (void)flag;
    return (void *)LoadLibraryA(path);
}
static void *am_dlsym(void *handle, const char *sym) {
    /* GetProcAddress returns FARPROC; route through uintptr_t to silence
     * the function-pointer-to-void* cast warning (same UB the POSIX path
     * already relies on). */
    return (void *)(uintptr_t)GetProcAddress((HMODULE)handle, sym);
}
static const char *am_dlerror(void) {
    static char buf[256];
    DWORD code = GetLastError();
    if (code == 0) return NULL;
    DWORD n = FormatMessageA(FORMAT_MESSAGE_FROM_SYSTEM | FORMAT_MESSAGE_IGNORE_INSERTS,
                             NULL, code, 0, buf, (DWORD)sizeof(buf) - 1, NULL);
    if (n == 0) snprintf(buf, sizeof(buf), "windows error %lu", (unsigned long)code);
    return buf;
}
#define dlopen(p, f) am_dlopen((p), (f))
#define dlsym(h, s) am_dlsym((h), (s))
#define dlerror() am_dlerror()
#else
#include <dlfcn.h>
#endif

static const OrtApi *g_api = NULL;
static void *g_dylib = NULL;

int am_onnx_load(const char *dylib_path, const char **err_out) {
    if (g_api != NULL) return 0;
    g_dylib = dlopen(dylib_path, RTLD_LAZY);
    if (!g_dylib) {
        if (err_out) *err_out = dlerror();
        return 1;
    }
    typedef OrtApiBase *(*get_api_base_fn)(void);
    get_api_base_fn get_base = (get_api_base_fn)dlsym(g_dylib, "OrtGetApiBase");
    if (!get_base) {
        if (err_out) *err_out = "OrtGetApiBase symbol missing";
        return 2;
    }
    OrtApiBase *base = get_base();
    if (!base) {
        if (err_out) *err_out = "OrtGetApiBase returned NULL";
        return 3;
    }
    g_api = base->GetApi(ORT_API_VERSION);
    if (!g_api) {
        if (err_out) *err_out = "incompatible ORT_API_VERSION";
        return 3;
    }
    return 0;
}

const char *am_onnx_version(void) {
    if (g_dylib == NULL) return NULL;
    typedef OrtApiBase *(*get_api_base_fn)(void);
    get_api_base_fn get_base = (get_api_base_fn)dlsym(g_dylib, "OrtGetApiBase");
    if (!get_base) return NULL;
    OrtApiBase *base = get_base();
    if (!base) return NULL;
    return base->GetVersionString();
}

/* --- env --- */
OrtStatus *am_create_env(int log_level, const char *name, OrtEnv **out) {
    return g_api->CreateEnv((OrtLoggingLevel)log_level, name, out);
}
OrtStatus *am_disable_env_telemetry(OrtEnv *env) {
    return g_api->DisableTelemetryEvents(env);
}
void am_release_env(OrtEnv *env) { g_api->ReleaseEnv(env); }

/* --- session options --- */
OrtStatus *am_create_session_options(OrtSessionOptions **out) {
    return g_api->CreateSessionOptions(out);
}
OrtStatus *am_session_options_set_intra_op_threads(OrtSessionOptions *o, int n) {
    return g_api->SetIntraOpNumThreads(o, n);
}
OrtStatus *am_session_options_set_inter_op_threads(OrtSessionOptions *o, int n) {
    return g_api->SetInterOpNumThreads(o, n);
}
OrtStatus *am_session_options_set_graph_opt_level(OrtSessionOptions *o, int level) {
    return g_api->SetSessionGraphOptimizationLevel(o, (GraphOptimizationLevel)level);
}
OrtStatus *am_session_options_append_coreml(OrtSessionOptions *o, unsigned int flags) {
    /* The CoreML provider lives in a separate header normally, but exposed
     * via SessionOptionsAppendExecutionProvider when CoreML is compiled in.
     * For arm64 Apple Silicon builds of ONNX Runtime, CoreML is bundled.
     * We probe at append time rather than build time. */
    typedef OrtStatus *(*ort_appender_fn)(OrtSessionOptions *, uint32_t);
    ort_appender_fn fn = (ort_appender_fn)dlsym(g_dylib,
        "OrtSessionOptionsAppendExecutionProvider_CoreML");
    if (!fn) {
        /* Fabricate an OrtStatus so the caller sees a normal error path. */
        return g_api->CreateStatus(ORT_NOT_IMPLEMENTED,
            "CoreML provider symbol not present in this libonnxruntime");
    }
    return fn(o, (uint32_t)flags);
}
void am_release_session_options(OrtSessionOptions *o) {
    g_api->ReleaseSessionOptions(o);
}

/* --- session --- */
OrtStatus *am_create_session(OrtEnv *env, const char *model_path,
                             OrtSessionOptions *opts, OrtSession **out) {
#ifdef _WIN32
    /* ONNX Runtime takes a wide-char (UTF-16) path on Windows — ORTCHAR_T is
     * wchar_t there, char on POSIX. The Go layer always hands us a UTF-8
     * char*, so convert before calling. A NULL OrtStatus means "success" in
     * ONNX's convention, so failures must return a real status (not NULL) or
     * the caller would dereference an uninitialised session. */
    int wlen = MultiByteToWideChar(CP_UTF8, 0, model_path, -1, NULL, 0);
    if (wlen <= 0) {
        return g_api->CreateStatus(ORT_INVALID_ARGUMENT, "model path utf8->utf16 conversion failed");
    }
    wchar_t *wpath = (wchar_t *)malloc((size_t)wlen * sizeof(wchar_t));
    if (!wpath) {
        return g_api->CreateStatus(ORT_FAIL, "out of memory converting model path");
    }
    MultiByteToWideChar(CP_UTF8, 0, model_path, -1, wpath, wlen);
    OrtStatus *st = g_api->CreateSession(env, wpath, opts, out);
    free(wpath);
    return st;
#else
    return g_api->CreateSession(env, model_path, opts, out);
#endif
}
OrtStatus *am_session_input_count(const OrtSession *s, size_t *out) {
    return g_api->SessionGetInputCount(s, out);
}
OrtStatus *am_session_output_count(const OrtSession *s, size_t *out) {
    return g_api->SessionGetOutputCount(s, out);
}
OrtStatus *am_session_input_name(const OrtSession *s, size_t idx, char **name) {
    OrtAllocator *alloc = NULL;
    OrtStatus *st = g_api->GetAllocatorWithDefaultOptions(&alloc);
    if (st) return st;
    return g_api->SessionGetInputName(s, idx, alloc, name);
}
OrtStatus *am_session_output_name(const OrtSession *s, size_t idx, char **name) {
    OrtAllocator *alloc = NULL;
    OrtStatus *st = g_api->GetAllocatorWithDefaultOptions(&alloc);
    if (st) return st;
    return g_api->SessionGetOutputName(s, idx, alloc, name);
}
void am_release_session(OrtSession *s) { g_api->ReleaseSession(s); }
void am_free_alloc(void *p) {
    OrtAllocator *alloc = NULL;
    if (g_api->GetAllocatorWithDefaultOptions(&alloc) == NULL && alloc) {
        alloc->Free(alloc, p);
    }
}

/* --- memory info / tensors --- */
OrtStatus *am_create_cpu_memory_info(OrtMemoryInfo **out) {
    return g_api->CreateCpuMemoryInfo(OrtArenaAllocator, OrtMemTypeDefault, out);
}
void am_release_memory_info(OrtMemoryInfo *m) {
    g_api->ReleaseMemoryInfo(m);
}
OrtStatus *am_create_tensor_with_data(OrtMemoryInfo *info,
                                      void *data, size_t data_len_bytes,
                                      const int64_t *shape, size_t shape_len,
                                      ONNXTensorElementDataType element_type,
                                      OrtValue **out) {
    return g_api->CreateTensorWithDataAsOrtValue(
        info, data, data_len_bytes, shape, shape_len, element_type, out);
}
OrtStatus *am_create_tensor_alloc(OrtMemoryInfo *info,
                                  const int64_t *shape, size_t shape_len,
                                  ONNXTensorElementDataType element_type,
                                  OrtValue **out) {
    OrtAllocator *alloc = NULL;
    OrtStatus *st = g_api->GetAllocatorWithDefaultOptions(&alloc);
    if (st) return st;
    return g_api->CreateTensorAsOrtValue(alloc, shape, shape_len, element_type, out);
}
OrtStatus *am_tensor_get_data(OrtValue *v, void **out) {
    return g_api->GetTensorMutableData(v, out);
}
OrtStatus *am_tensor_get_dims(const OrtValue *v, int64_t *dims_buf, size_t buf_len, size_t *out_len) {
    OrtTensorTypeAndShapeInfo *info = NULL;
    OrtStatus *st = g_api->GetTensorTypeAndShape(v, &info);
    if (st) return st;
    size_t n = 0;
    st = g_api->GetDimensionsCount(info, &n);
    if (st) {
        g_api->ReleaseTensorTypeAndShapeInfo(info);
        return st;
    }
    if (n > buf_len) n = buf_len;
    st = g_api->GetDimensions(info, dims_buf, n);
    g_api->ReleaseTensorTypeAndShapeInfo(info);
    if (st) return st;
    if (out_len) *out_len = n;
    return NULL;
}
OrtStatus *am_tensor_element_type(const OrtValue *v, ONNXTensorElementDataType *out) {
    OrtTensorTypeAndShapeInfo *info = NULL;
    OrtStatus *st = g_api->GetTensorTypeAndShape(v, &info);
    if (st) return st;
    st = g_api->GetTensorElementType(info, out);
    g_api->ReleaseTensorTypeAndShapeInfo(info);
    return st;
}
void am_release_value(OrtValue *v) { g_api->ReleaseValue(v); }

/* --- run --- */
OrtStatus *am_create_run_options(OrtRunOptions **out) {
    return g_api->CreateRunOptions(out);
}
OrtStatus *am_run_options_set_terminate(OrtRunOptions *o) {
    return g_api->RunOptionsSetTerminate(o);
}
OrtStatus *am_run_options_clear_terminate(OrtRunOptions *o) {
    return g_api->RunOptionsUnsetTerminate(o);
}
void am_release_run_options(OrtRunOptions *o) {
    g_api->ReleaseRunOptions(o);
}
OrtStatus *am_session_run(OrtSession *s, const OrtRunOptions *opts,
                          const char *const *input_names, const OrtValue *const *inputs,
                          size_t input_count,
                          const char *const *output_names, OrtValue **outputs,
                          size_t output_count) {
    return g_api->Run(s, opts, input_names, inputs, input_count,
                      output_names, output_count, outputs);
}

/* --- model metadata --- */
OrtStatus *am_session_get_metadata(const OrtSession *s, OrtModelMetadata **out) {
    return g_api->SessionGetModelMetadata(s, out);
}
OrtStatus *am_metadata_producer_name(const OrtModelMetadata *m, char **out) {
    OrtAllocator *alloc = NULL;
    OrtStatus *st = g_api->GetAllocatorWithDefaultOptions(&alloc);
    if (st) return st;
    return g_api->ModelMetadataGetProducerName(m, alloc, out);
}
OrtStatus *am_metadata_graph_name(const OrtModelMetadata *m, char **out) {
    OrtAllocator *alloc = NULL;
    OrtStatus *st = g_api->GetAllocatorWithDefaultOptions(&alloc);
    if (st) return st;
    return g_api->ModelMetadataGetGraphName(m, alloc, out);
}
OrtStatus *am_metadata_description(const OrtModelMetadata *m, char **out) {
    OrtAllocator *alloc = NULL;
    OrtStatus *st = g_api->GetAllocatorWithDefaultOptions(&alloc);
    if (st) return st;
    return g_api->ModelMetadataGetDescription(m, alloc, out);
}
OrtStatus *am_metadata_version(const OrtModelMetadata *m, int64_t *out) {
    return g_api->ModelMetadataGetVersion(m, out);
}
void am_release_metadata(OrtModelMetadata *m) {
    g_api->ReleaseModelMetadata(m);
}

/* --- status --- */
const char *am_status_message(const OrtStatus *s) {
    return g_api->GetErrorMessage(s);
}
void am_release_status(OrtStatus *s) {
    g_api->ReleaseStatus(s);
}
