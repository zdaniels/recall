//go:build darwin

package embed

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework NaturalLanguage

#include <stdlib.h>
#import <Foundation/Foundation.h>
#import <NaturalLanguage/NaturalLanguage.h>

// Module-scope cache. NLEmbedding sentence embedder is thread-safe and
// reusable; first lookup is the slow path (~3ms), subsequent calls are ~5ms.
static NLEmbedding *_apple_embedder = nil;
static dispatch_once_t _apple_init_once;
static int _apple_init_failed = 0;

static void apple_init(void) {
    dispatch_once(&_apple_init_once, ^{
        @autoreleasepool {
            NLEmbedding *e = [NLEmbedding sentenceEmbeddingForLanguage:NLLanguageEnglish];
            if (!e) {
                _apple_init_failed = 1;
                return;
            }
            _apple_embedder = e;
        }
    });
}

static int apple_dim(void) {
    apple_init();
    if (_apple_init_failed) return 0;
    return (int)_apple_embedder.dimension;
}

// apple_embed embeds a UTF-8 string and writes its vector to *out (length
// apple_dim()). Returns 0 on success, non-zero on error. Caller must
// pre-allocate `out` of length apple_dim().
static int apple_embed(const char *cstr, float *out, int out_len) {
    apple_init();
    if (_apple_init_failed) return 1;
    @autoreleasepool {
        NSString *s = [NSString stringWithUTF8String:cstr];
        if (!s) return 2;
        NSArray<NSNumber *> *v = [_apple_embedder vectorForString:s];
        if (!v || v.count == 0) return 3;
        NSUInteger n = v.count;
        if ((int)n > out_len) n = out_len;
        for (NSUInteger i = 0; i < n; i++) {
            out[i] = [v[i] floatValue];
        }
        for (NSUInteger i = n; i < (NSUInteger)out_len; i++) {
            out[i] = 0.0f;
        }
        return 0;
    }
}
*/
import "C"

import (
	"context"
	"fmt"
	"sync"
	"unsafe"
)

const appleAvailable = true

type appleEmbedder struct {
	dim int
	// vectorForString: itself is documented thread-safe, but we serialise
	// to be conservative — the cost is negligible vs the embedding work.
	mu sync.Mutex
}

func newApple() (Embedder, error) {
	dim := int(C.apple_dim())
	if dim == 0 {
		return nil, fmt.Errorf("apple sentence embedder unavailable for English (NLEmbedding returned nil)")
	}
	return &appleEmbedder{dim: dim}, nil
}

func (a *appleEmbedder) Name() string { return "apple-nlembedding-sentence-en" }
func (a *appleEmbedder) Dim() int     { return a.dim }

func (a *appleEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("empty text")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	cstr := C.CString(text)
	defer C.free(unsafe.Pointer(cstr))

	out := make([]float32, a.dim)
	rc := C.apple_embed(cstr, (*C.float)(unsafe.Pointer(&out[0])), C.int(a.dim))
	switch rc {
	case 0:
		return out, nil
	case 1:
		return nil, fmt.Errorf("apple embedder uninitialised")
	case 2:
		return nil, fmt.Errorf("invalid utf-8")
	case 3:
		return nil, fmt.Errorf("nil vector — string may be unsupported (e.g. all whitespace)")
	default:
		return nil, fmt.Errorf("apple embed rc=%d", rc)
	}
}
