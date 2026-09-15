/* Flat C wrappers around the OrtApi function table.
 *
 * The ONNX Runtime C API is exposed as a struct of function pointers
 * (OrtApi). CGO can call function pointers but it's painful — every call
 * needs a manual cast. Wrapping each entry in a flat C function keeps the
 * Go side terse and easy to audit.
 *
 * Scope: ~25 wrappers covering the operations recall uses today
 * (Init/CreateEnv/CreateSession/Run/Tensor) plus a forward-looking set
 * we're likely to need (model metadata, input/output introspection,
 * Apple Neural Engine via CoreML provider, telemetry control, run
 * cancellation, threading + graph optimisation tuning).
 *
 * All wrappers return an OrtStatus* — NULL on success, otherwise the
 * caller must read GetErrorMessage() and ReleaseStatus(). Translating
 * to Go's idiomatic (value, error) happens on the Go side.
 *
 * Memory ownership: every Create-XXX or Get-XXX-Name call returns a heap
 * object the caller must Release. Run() outputs are written into
 * pre-allocated tensors; the user owns those. */

#ifndef AGENT_MEMORY_ONNX_BRIDGE_H
#define AGENT_MEMORY_ONNX_BRIDGE_H

#include "onnxruntime_c_api.h"

/* ------ init / global ------ */

/* Load libonnxruntime.dylib via dlopen and capture the OrtApi pointer.
 * Idempotent: subsequent calls are no-ops. Returns 0 on success;
 * negative on failure (1: dlopen, 2: dlsym, 3: incompatible api version).
 * If non-zero, *err_out is filled with a static C string. */
int am_onnx_load(const char *dylib_path, const char **err_out);
const char *am_onnx_version(void);

/* ------ env / session-options / session ------ */

OrtStatus *am_create_env(int log_level, const char *name, OrtEnv **out);
OrtStatus *am_disable_env_telemetry(OrtEnv *env);
void am_release_env(OrtEnv *env);

OrtStatus *am_create_session_options(OrtSessionOptions **out);
OrtStatus *am_session_options_set_intra_op_threads(OrtSessionOptions *o, int n);
OrtStatus *am_session_options_set_inter_op_threads(OrtSessionOptions *o, int n);
OrtStatus *am_session_options_set_graph_opt_level(OrtSessionOptions *o, int level);
OrtStatus *am_session_options_append_coreml(OrtSessionOptions *o, unsigned int flags);
void am_release_session_options(OrtSessionOptions *o);

OrtStatus *am_create_session(OrtEnv *env, const char *model_path,
                             OrtSessionOptions *opts, OrtSession **out);
OrtStatus *am_session_input_count(const OrtSession *s, size_t *out);
OrtStatus *am_session_output_count(const OrtSession *s, size_t *out);
/* Caller must free *name with am_free_alloc when done. */
OrtStatus *am_session_input_name(const OrtSession *s, size_t idx, char **name);
OrtStatus *am_session_output_name(const OrtSession *s, size_t idx, char **name);
void am_release_session(OrtSession *s);
void am_free_alloc(void *p);

/* ------ memory info / tensors ------ */

OrtStatus *am_create_cpu_memory_info(OrtMemoryInfo **out);
void am_release_memory_info(OrtMemoryInfo *m);

/* Create a tensor from caller-owned data; data lifetime must outlive the
 * tensor (caller is responsible). element_type uses ONNX_TENSOR_ELEMENT_DATA_TYPE_*. */
OrtStatus *am_create_tensor_with_data(OrtMemoryInfo *info,
                                      void *data, size_t data_len_bytes,
                                      const int64_t *shape, size_t shape_len,
                                      ONNXTensorElementDataType element_type,
                                      OrtValue **out);

/* Create an empty tensor allocated by the runtime; caller writes via GetTensorMutableData. */
OrtStatus *am_create_tensor_alloc(OrtMemoryInfo *info,
                                  const int64_t *shape, size_t shape_len,
                                  ONNXTensorElementDataType element_type,
                                  OrtValue **out);

OrtStatus *am_tensor_get_data(OrtValue *v, void **out);
OrtStatus *am_tensor_get_dims(const OrtValue *v, int64_t *dims_buf, size_t buf_len, size_t *out_len);
OrtStatus *am_tensor_element_type(const OrtValue *v, ONNXTensorElementDataType *out);
void am_release_value(OrtValue *v);

/* ------ run ------ */

OrtStatus *am_create_run_options(OrtRunOptions **out);
OrtStatus *am_run_options_set_terminate(OrtRunOptions *o);
OrtStatus *am_run_options_clear_terminate(OrtRunOptions *o);
void am_release_run_options(OrtRunOptions *o);

OrtStatus *am_session_run(OrtSession *s, const OrtRunOptions *opts,
                          const char *const *input_names, const OrtValue *const *inputs,
                          size_t input_count,
                          const char *const *output_names, OrtValue **outputs,
                          size_t output_count);

/* ------ model metadata ------ */

OrtStatus *am_session_get_metadata(const OrtSession *s, OrtModelMetadata **out);
OrtStatus *am_metadata_producer_name(const OrtModelMetadata *m, char **out);
OrtStatus *am_metadata_graph_name(const OrtModelMetadata *m, char **out);
OrtStatus *am_metadata_description(const OrtModelMetadata *m, char **out);
OrtStatus *am_metadata_version(const OrtModelMetadata *m, int64_t *out);
void am_release_metadata(OrtModelMetadata *m);

/* ------ status / errors ------ */

const char *am_status_message(const OrtStatus *s);
void am_release_status(OrtStatus *s);

#endif /* AGENT_MEMORY_ONNX_BRIDGE_H */
