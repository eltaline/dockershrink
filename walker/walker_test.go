package walker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// buildTar creates an in-memory tar archive from a list of entries.
func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Size:     int64(len(e.body)),
			Mode:     e.mode,
			Typeflag: e.typeflag,
			Linkname: e.link,
		}
		if hdr.Typeflag == 0 && hdr.Mode == 0 {
			hdr.Mode = 0644
		}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %s: %v", e.name, err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write tar body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	name     string
	body     string
	mode     int64
	typeflag byte
	link     string
}

func gzipData(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zstdData(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

func TestWalkPlainTar(t *testing.T) {
	raw := buildTar(t, []tarEntry{
		{name: "bin/app", body: "hello"},
		{name: "etc/config.yml", body: "key: value"},
	})

	ls, err := Walk(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if ls.Compression != CompressionNone {
		t.Errorf("compression = %v, want none", ls.Compression)
	}
	if ls.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2", ls.FileCount)
	}
	if ls.TotalSize != 15 {
		t.Errorf("TotalSize = %d, want 15", ls.TotalSize)
	}
	if len(ls.Whiteouts) != 0 {
		t.Errorf("Whiteouts = %d, want 0", len(ls.Whiteouts))
	}
}

func TestWalkGzip(t *testing.T) {
	raw := buildTar(t, []tarEntry{
		{name: "data.txt", body: "compressed"},
	})
	gz := gzipData(t, raw)

	ls, err := Walk(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if ls.Compression != CompressionGzip {
		t.Errorf("compression = %v, want gzip", ls.Compression)
	}
	if ls.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1", ls.FileCount)
	}
	if ls.TotalSize != 10 {
		t.Errorf("TotalSize = %d, want 10", ls.TotalSize)
	}
}

func TestWalkZstd(t *testing.T) {
	raw := buildTar(t, []tarEntry{
		{name: "zstd-file.bin", body: "zstd content here"},
	})
	zd := zstdData(t, raw)

	ls, err := Walk(bytes.NewReader(zd))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if ls.Compression != CompressionZstd {
		t.Errorf("compression = %v, want zstd", ls.Compression)
	}
	if ls.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1", ls.FileCount)
	}
	if ls.TotalSize != 17 {
		t.Errorf("TotalSize = %d, want 17", ls.TotalSize)
	}
}

func TestWalkWhiteoutFile(t *testing.T) {
	raw := buildTar(t, []tarEntry{
		{name: "usr/bin/app", body: "binary"},
		{name: "usr/bin/.wh.old-app", body: ""},
	})

	ls, err := Walk(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if ls.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1 (whiteout excluded)", ls.FileCount)
	}
	if len(ls.Whiteouts) != 1 {
		t.Fatalf("Whiteouts = %d, want 1", len(ls.Whiteouts))
	}

	wo := ls.Whiteouts[0]
	if !wo.IsWhiteout {
		t.Error("whiteout entry should have IsWhiteout=true")
	}
	if wo.IsOpaqueDir {
		t.Error("whiteout entry should have IsOpaqueDir=false")
	}
	if wo.WhiteoutTarget != "usr/bin/old-app" {
		t.Errorf("WhiteoutTarget = %q, want %q", wo.WhiteoutTarget, "usr/bin/old-app")
	}
}

func TestWalkWhiteoutRootLevel(t *testing.T) {
	raw := buildTar(t, []tarEntry{
		{name: ".wh.removed-file", body: ""},
	})

	ls, err := Walk(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(ls.Whiteouts) != 1 {
		t.Fatalf("Whiteouts = %d, want 1", len(ls.Whiteouts))
	}
	if ls.Whiteouts[0].WhiteoutTarget != "removed-file" {
		t.Errorf("WhiteoutTarget = %q, want %q", ls.Whiteouts[0].WhiteoutTarget, "removed-file")
	}
}

func TestWalkOpaqueWhiteout(t *testing.T) {
	raw := buildTar(t, []tarEntry{
		{name: "var/cache/.wh..wh..opq", body: ""},
		{name: "var/cache/new-file", body: "fresh"},
	})

	ls, err := Walk(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if ls.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1", ls.FileCount)
	}
	if len(ls.Whiteouts) != 1 {
		t.Fatalf("Whiteouts = %d, want 1", len(ls.Whiteouts))
	}

	wo := ls.Whiteouts[0]
	if !wo.IsOpaqueDir {
		t.Error("opaque whiteout should have IsOpaqueDir=true")
	}
	if wo.WhiteoutTarget != "var/cache" {
		t.Errorf("WhiteoutTarget = %q, want %q", wo.WhiteoutTarget, "var/cache")
	}
}

func TestBuildIndex(t *testing.T) {
	layer0 := LayerSummary{
		Index:     0,
		TotalSize: 100,
		FileCount: 2,
		Files: []FileEntry{
			{Path: "bin/app", Size: 60},
			{Path: "etc/config", Size: 40},
		},
	}
	layer1 := LayerSummary{
		Index:     1,
		TotalSize: 50,
		FileCount: 1,
		Files: []FileEntry{
			{Path: "bin/app", Size: 50},
		},
	}

	idx := BuildIndex([]LayerSummary{layer0, layer1})

	if idx.TotalSize != 150 {
		t.Errorf("TotalSize = %d, want 150", idx.TotalSize)
	}
	if idx.TotalFiles != 3 {
		t.Errorf("TotalFiles = %d, want 3", idx.TotalFiles)
	}
	refs := idx.FilesByPath["bin/app"]
	if len(refs) != 2 {
		t.Fatalf("FilesByPath[bin/app] has %d refs, want 2", len(refs))
	}
	if refs[0].LayerIndex != 0 || refs[1].LayerIndex != 1 {
		t.Errorf("layer indices = %d,%d; want 0,1", refs[0].LayerIndex, refs[1].LayerIndex)
	}
}

func TestEffectiveFiles(t *testing.T) {
	layer0 := LayerSummary{
		Index:     0,
		TotalSize: 100,
		FileCount: 3,
		Files: []FileEntry{
			{Path: "bin/app", Size: 50},
			{Path: "bin/old", Size: 30},
			{Path: "etc/conf", Size: 20},
		},
	}
	layer1 := LayerSummary{
		Index:     1,
		TotalSize: 60,
		FileCount: 1,
		Files: []FileEntry{
			{Path: "bin/.wh.old", IsWhiteout: true, WhiteoutTarget: "bin/old"},
			{Path: "bin/app", Size: 60},
		},
		Whiteouts: []FileEntry{
			{Path: "bin/.wh.old", IsWhiteout: true, WhiteoutTarget: "bin/old"},
		},
	}

	idx := BuildIndex([]LayerSummary{layer0, layer1})
	eff := idx.EffectiveFiles()

	if _, ok := eff["bin/old"]; ok {
		t.Error("bin/old should be deleted by whiteout")
	}
	if ref, ok := eff["bin/app"]; !ok {
		t.Error("bin/app should exist")
	} else if ref.LayerIndex != 1 {
		t.Errorf("bin/app should come from layer 1, got %d", ref.LayerIndex)
	}
	if _, ok := eff["etc/conf"]; !ok {
		t.Error("etc/conf should still exist")
	}
}

func TestEffectiveFilesOpaqueWhiteout(t *testing.T) {
	layer0 := LayerSummary{
		Index:     0,
		TotalSize: 50,
		FileCount: 2,
		Files: []FileEntry{
			{Path: "var/cache/a", Size: 20},
			{Path: "var/cache/b", Size: 30},
		},
	}
	layer1 := LayerSummary{
		Index:     1,
		TotalSize: 10,
		FileCount: 1,
		Files: []FileEntry{
			{Path: "var/cache/.wh..wh..opq", IsWhiteout: true, IsOpaqueDir: true, WhiteoutTarget: "var/cache"},
			{Path: "var/cache/new", Size: 10},
		},
		Whiteouts: []FileEntry{
			{Path: "var/cache/.wh..wh..opq", IsWhiteout: true, IsOpaqueDir: true, WhiteoutTarget: "var/cache"},
		},
	}

	idx := BuildIndex([]LayerSummary{layer0, layer1})
	eff := idx.EffectiveFiles()

	if _, ok := eff["var/cache/a"]; ok {
		t.Error("var/cache/a should be removed by opaque whiteout")
	}
	if _, ok := eff["var/cache/b"]; ok {
		t.Error("var/cache/b should be removed by opaque whiteout")
	}
	if _, ok := eff["var/cache/new"]; !ok {
		t.Error("var/cache/new should exist (added in same layer after opaque)")
	}
}

func TestDetectCompression(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want Compression
	}{
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00}, CompressionGzip},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}, CompressionZstd},
		{"plain", []byte{0x00, 0x00, 0x00, 0x00}, CompressionNone},
		{"short", []byte{0x42}, CompressionNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comp, r, err := detectCompression(bytes.NewReader(tt.data))
			if err != nil {
				t.Fatalf("detectCompression: %v", err)
			}
			if comp != tt.want {
				t.Errorf("got %v, want %v", comp, tt.want)
			}
			// Verify the reader still has the data.
			rest, _ := io.ReadAll(r)
			if !bytes.Equal(rest, tt.data) {
				t.Error("peeked reader lost data")
			}
		})
	}
}

func TestCompressionString(t *testing.T) {
	if CompressionNone.String() != "none" {
		t.Errorf("none = %q", CompressionNone.String())
	}
	if CompressionGzip.String() != "gzip" {
		t.Errorf("gzip = %q", CompressionGzip.String())
	}
	if CompressionZstd.String() != "zstd" {
		t.Errorf("zstd = %q", CompressionZstd.String())
	}
}

func TestWalkEmptyTar(t *testing.T) {
	// An empty tar is just the end-of-archive marker (two 512-byte zero blocks).
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.Close()

	ls, err := Walk(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Walk empty tar: %v", err)
	}
	if ls.FileCount != 0 {
		t.Errorf("FileCount = %d, want 0", ls.FileCount)
	}
	if ls.TotalSize != 0 {
		t.Errorf("TotalSize = %d, want 0", ls.TotalSize)
	}
}

func TestWalkSymlink(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{
		Name:     "usr/bin/link",
		Typeflag: tar.TypeSymlink,
		Linkname: "/usr/bin/target",
		Mode:     0777,
	})
	tw.Close()

	ls, err := Walk(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if ls.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1", ls.FileCount)
	}
	if ls.Files[0].Link != "/usr/bin/target" {
		t.Errorf("Link = %q, want /usr/bin/target", ls.Files[0].Link)
	}
}
