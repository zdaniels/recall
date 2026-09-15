package embed

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Asset paths and download metadata for the on-device ONNX embedder.
//
// All downloaded artifacts have pinned SHA256 hashes; verification fails
// loudly rather than silently using an unexpected file. CISO-friendly:
// any tampering on huggingface.co or microsoft/onnxruntime's CDN at
// download time is detected before the file lands on disk.
//
// Disk layout:
//
//	~/.local/share/recall/
//	  runtime/libonnxruntime.dylib
//	  models/all-MiniLM-L6-v2/
//	    model_qint8_arm64.onnx       (~22 MiB, quantized for Apple Silicon)
//	    tokenizer.json                full HF tokenizer config (vocab + rules)
//	    config.json                   model metadata (max_seq_length etc)

const (
	OrtVersion        = "1.25.1"
	ModelName         = "all-MiniLM-L6-v2"
	ModelFile         = "model_qint8_arm64.onnx"
	TokenizerFile     = "tokenizer.json"
	ConfigFile        = "config.json"
	HuggingFaceRepo   = "sentence-transformers/all-MiniLM-L6-v2"
	HuggingFaceBranch = "main"
)

// Pinned SHA256 hashes — captured on the build host and verified against
// every downloaded or side-loaded copy. Update only when intentionally
// upgrading; mismatches always cause `recall download-model` and
// `recall install-model` to refuse the file. Run `shasum -a 256 <file>`
// to recompute when bumping.
const (
	// Model assets are platform-independent: the qint8 ONNX graph,
	// tokenizer, and config are identical bytes on every OS/arch (ONNX
	// Runtime's CPU EP runs the qint8 ops on x64 and arm64 alike — the
	// "_arm64" in the model filename is just HF's export name, not a
	// hard dependency). Only the ONNX Runtime library itself is
	// platform-specific; its hashes live in the ortReleases table below.
	ModelSHA256     = "4278337fd0ff3c68bfb6291042cad8ab363e1d9fbc43dcb499fe91c871902474"
	TokenizerSHA256 = "be50c3628f2bf5bb5e3a7f17b1f74611b2561a3a27eeab05e5aa30f411572037"
	ConfigSHA256    = "953f9c0d463486b10a6871cc2fd59f223b2c70184f49815e7efbcab5d8908b41"
)

// ortRelease pins one platform's ONNX Runtime download: the GitHub
// release tarball, its sha256, and the sha256 of the versioned shared
// library extracted from it. Both are verified — the tarball on
// download, the extracted lib again at load time (defence in depth).
type ortRelease struct {
	tarballURL string
	tarballSHA string
	libSHA     string
	subdir     string // top-level directory inside the tarball
	libName    string // versioned lib filename under <subdir>/lib/
}

// ortReleases is keyed by "<GOOS>/<GOARCH>". To support a new platform,
// add a row with hashes recomputed via `shasum -a 256` against the
// official Microsoft release archive + the lib extracted from it.
// macOS ships a .dylib (.tgz), Linux a versioned .so (.tgz), Windows an
// unversioned onnxruntime.dll (.zip — see downloadRuntime). On Windows
// the C bridge loads it via LoadLibrary instead of dlopen (bridge.c).
var ortReleases = map[string]ortRelease{
	"darwin/arm64": {
		tarballURL: ortURL("osx-arm64"),
		tarballSHA: "18987ec3187b5f29ba798109750f6135060560ad4e0a52678fcc753ee8fb3091",
		libSHA:     "c009754f5c160014ee302b28f44b888ad95c841d06e343fb953f5894beee81a4",
		subdir:     "onnxruntime-osx-arm64-" + OrtVersion,
		libName:    "libonnxruntime." + OrtVersion + ".dylib",
	},
	"linux/amd64": {
		tarballURL: ortURL("linux-x64"),
		tarballSHA: "eb566a49cfc49ef0642f809b69340b5bb656c7c4905ba873526d226f2c005816",
		libSHA:     "7a7c63fec274e9577477a0158a03a205e3ed9320b032a2df2b2a01b6ae1d3b94",
		subdir:     "onnxruntime-linux-x64-" + OrtVersion,
		libName:    "libonnxruntime.so." + OrtVersion,
	},
	"linux/arm64": {
		tarballURL: ortURL("linux-aarch64"),
		tarballSHA: "daa71b56b00c4ab34798a3d96ca41a32ece4d3e302dc2386d3cca83fd4491214",
		libSHA:     "e34095ad2791f1f05ed17324ede7873b4fdb4c136b3ba3f1dd5093d2e76d4192",
		subdir:     "onnxruntime-linux-aarch64-" + OrtVersion,
		libName:    "libonnxruntime.so." + OrtVersion,
	},
	// Windows ships a .zip (not .tgz) and an UNVERSIONED onnxruntime.dll
	// (no "1.25.1" in the filename, unlike the .dylib/.so). downloadRuntime
	// detects the .zip suffix and extracts via archive/zip.
	"windows/amd64": {
		tarballURL: ortURLExt("win-x64", "zip"),
		tarballSHA: "33f2e8a63774811f99a5fc224cac32f4eed8c27643d46c6cc685319fa8f18019",
		libSHA:     "373d4cb6fe92966125c4ac0d07b190ce4863e45251438f0ec087d4576b862918",
		subdir:     "onnxruntime-win-x64-" + OrtVersion,
		libName:    "onnxruntime.dll",
	},
	"windows/arm64": {
		tarballURL: ortURLExt("win-arm64", "zip"),
		tarballSHA: "57d36a7b7607967cc1e7d758b2104ebdcfac19e2bb3aa8683914dfeeff75f068",
		libSHA:     "94c7a25a781d7b599abb96a2b9644a85ea20feb6461cfe14cbce7d776843e9b1",
		subdir:     "onnxruntime-win-arm64-" + OrtVersion,
		libName:    "onnxruntime.dll",
	},
}

func ortURL(plat string) string { return ortURLExt(plat, "tgz") }

func ortURLExt(plat, ext string) string {
	return fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-%s-%s.%s",
		OrtVersion, plat, OrtVersion, ext)
}

// currentOrtRelease returns the pinned release for the running platform.
func currentOrtRelease() (ortRelease, bool) {
	r, ok := ortReleases[runtime.GOOS+"/"+runtime.GOARCH]
	return r, ok
}

func AssetsRoot() string {
	// RECALL_HOME is the canonical override; AGENT_MEMORY_HOME is the
	// legacy env name from the pre-rename era, honored for one release.
	if v := os.Getenv("RECALL_HOME"); v != "" {
		return v
	}
	if v := os.Getenv("AGENT_MEMORY_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "recall")
}

func ModelDir() string      { return filepath.Join(AssetsRoot(), "models", ModelName) }
func ModelPath() string     { return filepath.Join(ModelDir(), ModelFile) }
func TokenizerPath() string { return filepath.Join(ModelDir(), TokenizerFile) }
func ConfigPath() string    { return filepath.Join(ModelDir(), ConfigFile) }

// runtimeLibName is the on-disk filename of the ONNX Runtime shared
// library on the current platform.
func runtimeLibName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libonnxruntime.dylib"
	case "windows":
		return "onnxruntime.dll"
	default:
		return "libonnxruntime.so"
	}
}

// RuntimeLibPath is where we install + load the ONNX Runtime shared
// library. (Formerly RuntimeDylibPath — renamed now that Linux uses
// .so and Windows .dll, not just macOS .dylib.)
func RuntimeLibPath() string { return filepath.Join(AssetsRoot(), "runtime", runtimeLibName()) }

// OnnxAssetsAvailable returns true when both the runtime library and the
// model files are present and non-empty. The ONNX embedder uses this to
// decide whether to initialise itself or fall back gracefully.
func OnnxAssetsAvailable() bool {
	for _, p := range []string{RuntimeLibPath(), ModelPath(), TokenizerPath()} {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

func hfURL(path string) string {
	return fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s",
		HuggingFaceRepo, HuggingFaceBranch, path)
}

// DownloadOnnxAssets fetches the runtime dylib + model files. Idempotent
// when force=false: skips anything already present and non-empty (after
// verifying its SHA256 against the pin). Writes progress to w.
func DownloadOnnxAssets(ctx context.Context, force bool, w io.Writer) error {
	if w == nil {
		w = io.Discard
	}
	// Runtime
	rel, ok := currentOrtRelease()
	if !ok {
		return fmt.Errorf("no prebuilt ONNX Runtime pinned for %s/%s — use --provider ollama, or side-load with `recall install-model --from DIR`",
			runtime.GOOS, runtime.GOARCH)
	}
	if !cached(RuntimeLibPath(), rel.libSHA, force, w) {
		if err := downloadRuntime(ctx, w); err != nil {
			return fmt.Errorf("runtime: %w", err)
		}
		if err := verifySHA256(RuntimeLibPath(), rel.libSHA); err != nil {
			os.Remove(RuntimeLibPath())
			return fmt.Errorf("runtime sha256: %w", err)
		}
	}
	// Model files
	files := []struct{ url, dest, sha string }{
		{hfURL("onnx/" + ModelFile), ModelPath(), ModelSHA256},
		{hfURL(TokenizerFile), TokenizerPath(), TokenizerSHA256},
		{hfURL(ConfigFile), ConfigPath(), ConfigSHA256},
	}
	for _, f := range files {
		if cached(f.dest, f.sha, force, w) {
			continue
		}
		if err := downloadFile(ctx, f.url, f.dest, w); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f.dest), err)
		}
		if err := verifySHA256(f.dest, f.sha); err != nil {
			os.Remove(f.dest)
			return fmt.Errorf("%s sha256: %w", filepath.Base(f.dest), err)
		}
		fmt.Fprintf(w, "  ✓ sha256 verified\n")
	}
	return nil
}

// InstallFromDir copies + verifies a side-loaded set of assets. Use when
// the security team mirrors the artifacts internally and hands users the
// directory rather than allowing internet downloads. The directory must
// contain libonnxruntime.dylib, model_qint8_arm64.onnx, tokenizer.json,
// and config.json — the loose layout produced by `recall download-model`
// at ~/.local/share/recall/, OR a flat directory.
//
// Each file is SHA256-verified against the pinned hash before it's
// promoted into place. Atomic per-file writes via tmp+rename.
func InstallFromDir(srcDir string, w io.Writer) error {
	if w == nil {
		w = io.Discard
	}
	if _, err := os.Stat(srcDir); err != nil {
		return fmt.Errorf("source dir: %w", err)
	}
	rel, ok := currentOrtRelease()
	if !ok {
		return fmt.Errorf("no prebuilt ONNX Runtime pinned for %s/%s — use --provider ollama", runtime.GOOS, runtime.GOARCH)
	}
	libName := runtimeLibName()
	type item struct {
		candidates []string
		dest       string
		sha        string
	}
	items := []item{
		{
			candidates: []string{
				filepath.Join(srcDir, libName),
				filepath.Join(srcDir, "runtime", libName),
			},
			dest: RuntimeLibPath(),
			sha:  rel.libSHA,
		},
		{
			candidates: []string{
				filepath.Join(srcDir, ModelFile),
				filepath.Join(srcDir, "models", ModelName, ModelFile),
			},
			dest: ModelPath(),
			sha:  ModelSHA256,
		},
		{
			candidates: []string{
				filepath.Join(srcDir, TokenizerFile),
				filepath.Join(srcDir, "models", ModelName, TokenizerFile),
			},
			dest: TokenizerPath(),
			sha:  TokenizerSHA256,
		},
		{
			candidates: []string{
				filepath.Join(srcDir, ConfigFile),
				filepath.Join(srcDir, "models", ModelName, ConfigFile),
			},
			dest: ConfigPath(),
			sha:  ConfigSHA256,
		},
	}
	for _, it := range items {
		var src string
		for _, c := range it.candidates {
			if fileExists(c) {
				src = c
				break
			}
		}
		if src == "" {
			return fmt.Errorf("could not find %s in %s (looked in: %v)",
				filepath.Base(it.dest), srcDir, it.candidates)
		}
		fmt.Fprintf(w, "↧ %s\n", src)
		if err := verifySHA256(src, it.sha); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(it.dest), err)
		}
		if err := copyFile(src, it.dest); err != nil {
			return fmt.Errorf("copy %s: %w", filepath.Base(it.dest), err)
		}
		fmt.Fprintf(w, "  → %s ✓ sha256 verified\n", it.dest)
	}
	return nil
}

// verifySHA256 reads the file and compares its SHA256 against the expected
// hex digest. Returns nil on match, error otherwise (including the actual
// hash so the user can investigate / pin the new version intentionally).
func verifySHA256(path, expectedHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expectedHex {
		return fmt.Errorf("hash mismatch — got %s, want %s", got, expectedHex)
	}
	return nil
}

// cached returns true (and prints "(cached)") when the file is already on
// disk and matches the pinned hash. force=true bypasses the cache.
func cached(path, sha string, force bool, w io.Writer) bool {
	if force || !fileExists(path) {
		return false
	}
	if err := verifySHA256(path, sha); err != nil {
		fmt.Fprintf(w, "⚠ %s: %v — redownloading\n", filepath.Base(path), err)
		return false
	}
	fmt.Fprintf(w, "✓ %s (cached, sha256 ok)\n", filepath.Base(path))
	return true
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// extractZipFile copies the single entry whose (slash-normalised) name
// equals wantRel out of the zip at zipPath, writing it to dst. Pure Go
// (archive/zip) so Windows doesn't need Expand-Archive or a zip-aware
// tar. Errors if the entry isn't found.
func extractZipFile(zipPath, wantRel, dst string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if filepath.ToSlash(f.Name) != wantRel {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("open zip entry %s: %w", wantRel, err)
		}
		defer rc.Close()
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer out.Close()
		// Cap the copy to guard against a malicious/oversized entry; the
		// real DLL is ~30 MiB, so 256 MiB is a generous ceiling.
		if _, err := io.Copy(out, io.LimitReader(rc, 256<<20)); err != nil {
			return err
		}
		return out.Close()
	}
	return fmt.Errorf("entry %s not found in %s", wantRel, filepath.Base(zipPath))
}

func downloadRuntime(ctx context.Context, w io.Writer) error {
	rel, ok := currentOrtRelease()
	if !ok {
		return fmt.Errorf("no prebuilt ONNX Runtime for %s/%s; install manually", runtime.GOOS, runtime.GOARCH)
	}
	if err := os.MkdirAll(filepath.Dir(RuntimeLibPath()), 0o755); err != nil {
		return err
	}

	isZip := strings.HasSuffix(rel.tarballURL, ".zip")
	archiveExt := ".tgz"
	if isZip {
		archiveExt = ".zip"
	}
	tmp := filepath.Join(os.TempDir(), "recall-ort"+archiveExt)
	if err := downloadFile(ctx, rel.tarballURL, tmp, w); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := verifySHA256(tmp, rel.tarballSHA); err != nil {
		return fmt.Errorf("ort archive sha256: %w", err)
	}
	fmt.Fprintf(w, "  ✓ archive sha256 verified\n")

	extractDir := filepath.Join(os.TempDir(), "recall-ort")
	os.RemoveAll(extractDir)
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(extractDir)

	// The library lives at <subdir>/lib/<libName> inside the archive.
	// Windows ships a .zip (extracted in pure Go so we don't depend on
	// Expand-Archive / a tar that understands zip); macOS + Linux ship a
	// .tgz extracted via the system tar (more reliable than archive/tar
	// for the versioned-symlink layout).
	wantRel := filepath.ToSlash(filepath.Join(rel.subdir, "lib", rel.libName))
	if isZip {
		if err := extractZipFile(tmp, wantRel, RuntimeLibPath()); err != nil {
			return err
		}
		return os.Chmod(RuntimeLibPath(), 0o755)
	}
	if err := exec.CommandContext(ctx, "tar", "-xzf", tmp, "-C", extractDir).Run(); err != nil {
		return fmt.Errorf("untar: %w", err)
	}
	src := filepath.Join(extractDir, rel.subdir, "lib", rel.libName)
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("locate runtime lib: %w", err)
	}
	defer in.Close()
	out, err := os.Create(RuntimeLibPath())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return os.Chmod(RuntimeLibPath(), 0o755)
}

func downloadFile(ctx context.Context, url, dest string, w io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(w, "↓ %s\n", url)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	fmt.Fprintf(w, "  → %s (%s)\n", dest, humanBytes(n))
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
