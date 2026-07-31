// Package walker provides layer traversal for OCI/Docker image layers.
// It handles gzip and zstd decompression, iterates tar entries, detects
// whiteout files (AUFS/OverlayFS deletion markers), and builds size indices
// at both layer and file granularity.
package walker

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Compression describes the detected compression format of a layer blob.
type Compression int

const (
	CompressionNone Compression = iota
	CompressionGzip
	CompressionZstd
)

func (c Compression) String() string {
	switch c {
	case CompressionGzip:
		return "gzip"
	case CompressionZstd:
		return "zstd"
	default:
		return "none"
	}
}

// whiteout constants per the OCI image spec.
const (
	whiteoutPrefix    = ".wh."
	whiteoutOpaqueDir = ".wh..wh..opq"
)

// FileEntry represents a single file inside a layer tar archive.
type FileEntry struct {
	Path           string // path inside the layer
	Size           int64  // uncompressed size in bytes
	Mode           uint32 // file mode bits
	Link           string // hard/symlink target
	IsWhiteout     bool   // true if this is a whiteout marker (.wh.*)
	IsOpaqueDir    bool   // true if this is an opaque whiteout (.wh..wh..opq)
	WhiteoutTarget string // for whiteouts: the path being deleted
}

// LayerSummary holds the result of walking a single layer.
type LayerSummary struct {
	Index       int         // layer position in the image
	DiffID      string      // diff ID (sha256:...)
	CreatedBy   string      // Dockerfile command that produced this layer
	Compression Compression // detected compression format
	TotalSize   int64       // sum of all file sizes (excluding whiteouts)
	FileCount   int         // number of regular (non-whiteout) files
	Files       []FileEntry // all entries including whiteouts
	Whiteouts   []FileEntry // whiteout entries only
}

// Index is a size index built from multiple walked layers.
type Index struct {
	Layers      []LayerSummary       // per-layer summaries
	TotalSize   int64                // sum of all layers
	TotalFiles  int                  // sum of all file counts
	FilesByPath map[string][]FileRef // path -> entries across layers
}

// FileRef points to a file entry within a specific layer.
type FileRef struct {
	LayerIndex int
	Entry      FileEntry
}

// detectCompression peeks at the first bytes of r to determine
// the compression format. It returns the detected format and a
// reader that replays the peeked bytes followed by the rest of r.
func detectCompression(r io.Reader) (Compression, io.Reader, error) {
	br := bufio.NewReaderSize(r, 4)
	hdr, err := br.Peek(4)
	if err != nil && len(hdr) < 2 {
		// If we can't even read 2 bytes, assume no compression (empty layer).
		return CompressionNone, br, nil
	}

	if len(hdr) >= 2 && hdr[0] == 0x1f && hdr[1] == 0x8b {
		return CompressionGzip, br, nil
	}
	if len(hdr) >= 4 && hdr[0] == 0x28 && hdr[1] == 0xb5 && hdr[2] == 0x2f && hdr[3] == 0xfd {
		return CompressionZstd, br, nil
	}

	return CompressionNone, br, nil
}

// decompress wraps r with the appropriate decompressor.
// The caller must call the returned cleanup function when done.
func decompress(r io.Reader, comp Compression) (io.Reader, func(), error) {
	switch comp {
	case CompressionGzip:
		gr, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip decompress: %w", err)
		}
		return gr, func() { gr.Close() }, nil
	case CompressionZstd:
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd decompress: %w", err)
		}
		return zr, func() { zr.Close() }, nil
	default:
		return r, func() {}, nil
	}
}

// Walk reads a (possibly compressed) layer stream and returns a LayerSummary.
// The reader should provide the raw layer blob; compression is auto-detected
// from magic bytes (gzip: 1f 8b, zstd: 28 b5 2f fd).
func Walk(r io.Reader) (*LayerSummary, error) {
	comp, peekReader, err := detectCompression(r)
	if err != nil {
		return nil, fmt.Errorf("detect compression: %w", err)
	}

	dr, cleanup, err := decompress(peekReader, comp)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	ls := &LayerSummary{
		Compression: comp,
	}

	tr := tar.NewReader(dr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}

		fe := FileEntry{
			Path: hdr.Name,
			Size: hdr.Size,
			Mode: uint32(hdr.Mode),
			Link: hdr.Linkname,
		}

		base := path.Base(hdr.Name)
		dir := path.Dir(hdr.Name)

		if base == whiteoutOpaqueDir {
			fe.IsWhiteout = true
			fe.IsOpaqueDir = true
			fe.WhiteoutTarget = dir
			fe.Size = 0
			ls.Whiteouts = append(ls.Whiteouts, fe)
		} else if strings.HasPrefix(base, whiteoutPrefix) {
			fe.IsWhiteout = true
			target := strings.TrimPrefix(base, whiteoutPrefix)
			if dir == "." {
				fe.WhiteoutTarget = target
			} else {
				fe.WhiteoutTarget = dir + "/" + target
			}
			fe.Size = 0
			ls.Whiteouts = append(ls.Whiteouts, fe)
		} else {
			ls.TotalSize += hdr.Size
			ls.FileCount++
		}

		ls.Files = append(ls.Files, fe)
	}

	return ls, nil
}

// BuildIndex constructs a unified Index from a slice of LayerSummary.
// It aggregates sizes and builds a per-path file reference map that
// shows which layers contribute to each path.
func BuildIndex(layers []LayerSummary) *Index {
	idx := &Index{
		Layers:      layers,
		FilesByPath: make(map[string][]FileRef),
	}

	for i := range layers {
		idx.TotalSize += layers[i].TotalSize
		idx.TotalFiles += layers[i].FileCount
		for _, fe := range layers[i].Files {
			idx.FilesByPath[fe.Path] = append(idx.FilesByPath[fe.Path], FileRef{
				LayerIndex: i,
				Entry:      fe,
			})
		}
	}

	return idx
}

// EffectiveFiles returns the final set of visible files after applying all
// whiteout deletions across the layer stack. Files are keyed by cleaned path;
// whiteout entries remove the corresponding paths, and opaque whiteouts
// remove all entries under the target directory from earlier layers.
func (idx *Index) EffectiveFiles() map[string]FileRef {
	effective := make(map[string]FileRef)

	for i, layer := range idx.Layers {
		// First apply opaque whiteouts: remove everything under the directory
		// from previous layers.
		for _, wo := range layer.Whiteouts {
			if wo.IsOpaqueDir {
				prefix := cleanDir(wo.WhiteoutTarget)
				for p := range effective {
					if strings.HasPrefix(p, prefix) {
						delete(effective, p)
					}
				}
			}
		}

		for _, fe := range layer.Files {
			p := cleanPath(fe.Path)
			if fe.IsWhiteout && !fe.IsOpaqueDir {
				delete(effective, cleanPath(fe.WhiteoutTarget))
			} else if !fe.IsWhiteout {
				effective[p] = FileRef{LayerIndex: i, Entry: fe}
			}
		}
	}

	return effective
}

// cleanPath normalizes a tar path for consistent lookup.
func cleanPath(p string) string {
	p = path.Clean(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return p
}

// cleanDir normalizes a directory path and ensures it ends with "/".
func cleanDir(p string) string {
	p = cleanPath(p)
	if p == "." || p == "" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}
