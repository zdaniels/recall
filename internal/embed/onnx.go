package embed

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/zdaniels/recall/internal/onnx"
)

// onnxEmbedder runs sentence-transformers/all-MiniLM-L6-v2 (or any BERT-style
// drop-in) via Microsoft's ONNX Runtime locally. Inputs are tokenized via
// the in-package WordPiece tokenizer, padded to a fixed length so we can
// keep one Session alive across many Embed() calls (avoids per-call
// session-creation cost ~ 100ms).
//
// Quality target: cosine similarity that meaningfully distinguishes
// technical paraphrases — "executable installation directory" should rank
// "binary at ~/.local/bin/recall" higher than "paste code".
//
// CGO surface: we call our own internal/onnx package, which is a thin
// wrapper around the official ONNX Runtime C API (vendored MIT header,
// runtime dylib downloaded + SHA256-pinned). No third-party Go bindings.
type onnxEmbedder struct {
	tokenizer *Tokenizer
	env       *onnx.Env
	opts      *onnx.SessionOptions
	mi        *onnx.MemoryInfo
	session   *onnx.Session
	maxLen    int
	dim       int

	// Pre-allocated tensors reused per call. The Go-side backing slices
	// must stay alive for the life of these tensors; we keep them on
	// the struct.
	idsBuf   []int64
	maskBuf  []int64
	typesBuf []int64

	inIDs   *onnx.Tensor
	inMask  *onnx.Tensor
	inTypes *onnx.Tensor
	output  *onnx.Tensor

	mu sync.Mutex
}

const onnxMaxLen = 128 // covers our decision lengths (typically <50 tokens)
const onnxDim = 384    // all-MiniLM-L6-v2 output dim

func newONNX() (Embedder, error) {
	if !OnnxAssetsAvailable() {
		return nil, fmt.Errorf("ONNX runtime + model not present at %s — run `recall download-model` first",
			AssetsRoot())
	}
	// Defence-in-depth: re-verify SHA256 at load time, not just at download
	// time. Catches any post-download tampering (file replaced on disk,
	// permissions abuse, supply-chain attack on the cache directory).
	rel, ok := currentOrtRelease()
	if !ok {
		return nil, fmt.Errorf("ONNX Runtime not available on this platform")
	}
	if err := verifySHA256(RuntimeLibPath(), rel.libSHA); err != nil {
		return nil, fmt.Errorf("runtime lib integrity check failed: %w", err)
	}
	if err := verifySHA256(ModelPath(), ModelSHA256); err != nil {
		return nil, fmt.Errorf("model integrity check failed: %w", err)
	}
	if err := verifySHA256(TokenizerPath(), TokenizerSHA256); err != nil {
		return nil, fmt.Errorf("tokenizer integrity check failed: %w", err)
	}

	if err := onnx.Load(RuntimeLibPath()); err != nil {
		return nil, fmt.Errorf("load ONNX runtime: %w", err)
	}

	tk, err := LoadTokenizer(TokenizerPath(), onnxMaxLen)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: %w", err)
	}

	env, err := onnx.NewEnv(onnx.LogWarning, "recall")
	if err != nil {
		return nil, err
	}
	opts, err := onnx.NewSessionOptions()
	if err != nil {
		env.Close()
		return nil, err
	}
	// Best-effort: turn on all graph optimisations. Threading defaults
	// (intra=cores, inter=1) are reasonable on Apple Silicon.
	_ = opts.SetGraphOptLevel(onnx.GraphOptAll)
	mi, err := onnx.NewCPUMemoryInfo()
	if err != nil {
		opts.Close()
		env.Close()
		return nil, err
	}

	session, err := onnx.NewSession(env, ModelPath(), opts)
	if err != nil {
		mi.Close()
		opts.Close()
		env.Close()
		return nil, fmt.Errorf("create session: %w", err)
	}

	// Pre-allocate input/output tensors. CreateTensorWithData (used here
	// for inputs) wraps Go-owned memory; we keep the slices on the struct
	// so they outlive the session.
	idsBuf := make([]int64, onnxMaxLen)
	maskBuf := make([]int64, onnxMaxLen)
	typesBuf := make([]int64, onnxMaxLen)

	shape := []int64{1, int64(onnxMaxLen)}
	inIDs, err := onnx.NewInt64Tensor(mi, shape, idsBuf)
	if err != nil {
		session.Close()
		mi.Close()
		opts.Close()
		env.Close()
		return nil, err
	}
	inMask, err := onnx.NewInt64Tensor(mi, shape, maskBuf)
	if err != nil {
		inIDs.Close()
		session.Close()
		mi.Close()
		opts.Close()
		env.Close()
		return nil, err
	}
	inTypes, err := onnx.NewInt64Tensor(mi, shape, typesBuf)
	if err != nil {
		inIDs.Close()
		inMask.Close()
		session.Close()
		mi.Close()
		opts.Close()
		env.Close()
		return nil, err
	}
	outShape := []int64{1, int64(onnxMaxLen), int64(onnxDim)}
	output, err := onnx.NewEmptyTensor(mi, outShape, onnx.TypeFloat32)
	if err != nil {
		inIDs.Close()
		inMask.Close()
		inTypes.Close()
		session.Close()
		mi.Close()
		opts.Close()
		env.Close()
		return nil, err
	}

	return &onnxEmbedder{
		tokenizer: tk,
		env:       env,
		opts:      opts,
		mi:        mi,
		session:   session,
		maxLen:    onnxMaxLen,
		dim:       onnxDim,
		idsBuf:    idsBuf,
		maskBuf:   maskBuf,
		typesBuf:  typesBuf,
		inIDs:     inIDs,
		inMask:    inMask,
		inTypes:   inTypes,
		output:    output,
	}, nil
}

func (o *onnxEmbedder) Name() string { return "onnx-all-MiniLM-L6-v2-q8" }
func (o *onnxEmbedder) Dim() int     { return o.dim }

func (o *onnxEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("empty text")
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	ids, mask, types := o.tokenizer.Encode(text)
	if len(ids) > o.maxLen {
		ids = ids[:o.maxLen]
		mask = mask[:o.maxLen]
		types = types[:o.maxLen]
	}

	// Write into the backing slices that the input tensors view directly.
	// PAD token id = 0; mask = 0 marks padded positions for mean-pooling.
	for i := 0; i < o.maxLen; i++ {
		if i < len(ids) {
			o.idsBuf[i] = ids[i]
			o.maskBuf[i] = mask[i]
			o.typesBuf[i] = types[i]
		} else {
			o.idsBuf[i] = 0
			o.maskBuf[i] = 0
			o.typesBuf[i] = 0
		}
	}

	if err := o.session.Run(
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]*onnx.Tensor{o.inIDs, o.inMask, o.inTypes},
		[]string{"last_hidden_state"},
		[]*onnx.Tensor{o.output},
	); err != nil {
		return nil, fmt.Errorf("inference: %w", err)
	}

	tokenEmb, err := o.output.Float32Data()
	if err != nil {
		return nil, err
	}

	// Mean-pool weighted by attention mask, then L2 normalise. Matches
	// sentence-transformers' default sentence embedding pooling.
	pooled := make([]float32, o.dim)
	var nMask int64
	for t := 0; t < o.maxLen; t++ {
		if o.maskBuf[t] == 0 {
			continue
		}
		nMask++
		base := t * o.dim
		for d := 0; d < o.dim; d++ {
			pooled[d] += tokenEmb[base+d]
		}
	}
	if nMask > 0 {
		inv := float32(1.0 / float64(nMask))
		for d := range pooled {
			pooled[d] *= inv
		}
	}
	var sq float64
	for _, v := range pooled {
		sq += float64(v) * float64(v)
	}
	if sq > 0 {
		inv := float32(1.0 / math.Sqrt(sq))
		for d := range pooled {
			pooled[d] *= inv
		}
	}
	return pooled, nil
}
