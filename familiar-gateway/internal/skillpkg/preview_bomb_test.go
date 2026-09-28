package skillpkg

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// The dry-run preview (the default import path, open to every user)
// extracts with the same caps as the import. It used to write every
// entry with no count or total cap, so previewing a 20MB zip of zeros
// wrote gigabytes to the temp dir before anything was checked.
func TestPreviewZip_RefusesWhatImportRefuses(t *testing.T) {
	s := &Store{}

	big := make([]byte, 3<<20) // two of these pass the per-file cap, not the total
	var bomb bytes.Buffer
	zw := zip.NewWriter(&bomb)
	for _, n := range []string{"SKILL.md", "data.bin"} {
		w, _ := zw.Create(n)
		w.Write(big)
	}
	zw.Close()
	if _, _, _, err := s.PreviewZip(bomb.Bytes(), nil); err == nil || !strings.Contains(err.Error(), "total cap") {
		t.Errorf("decompression bomb: err = %v, want the total cap", err)
	}

	var many bytes.Buffer
	zw = zip.NewWriter(&many)
	for i := 0; i <= maxZipFiles; i++ {
		w, _ := zw.Create(fmt.Sprintf("f%d.txt", i))
		w.Write([]byte("x"))
	}
	zw.Close()
	if _, _, _, err := s.PreviewZip(many.Bytes(), nil); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("over-count archive: err = %v, want the entry cap", err)
	}
}
