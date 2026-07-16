package media

// Media store tests — dedicated `media_test` Postgres schema (the
// shared test DB truncates elsewhere; see skillpkg for the pattern).
// Image fixtures are generated in-process so no binary blobs live in
// the repo.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/familiar/gateway/internal/db"
)

func storeForTest(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("FAMILIAR_TEST_DSN")
	if dsn == "" {
		t.Skip("skipping: FAMILIAR_TEST_DSN not set")
	}
	ctx := context.Background()
	adminPool, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = adminPool.Close() })
	if _, err := adminPool.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS media_test`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err := db.Open(dsn + sep + "options=" + url.QueryEscape("-csearch_path=media_test,public"))
	if err != nil {
		t.Fatalf("db.Open scoped: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s, err := NewStore(pool, t.TempDir(), 5<<20)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// seedPage creates user → book → page and returns the page id.
func seedPage(t *testing.T, s *Store, user string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.ExecContext(ctx, `
		INSERT INTO users (id, display_name, status, role)
		VALUES ($1, $1, 'approved', 'user') ON CONFLICT (id) DO NOTHING`, user); err != nil {
		t.Fatal(err)
	}
	var bookID string
	if err := s.pool.QueryRowContext(ctx, `
		INSERT INTO books (slug, name, created_by)
		VALUES ($1, $1, $2) RETURNING id::text`,
		fmt.Sprintf("mb-%d", time.Now().UnixNano()%1_000_000_000), user).Scan(&bookID); err != nil {
		t.Fatalf("seed book: %v", err)
	}
	var pageID string
	if err := s.pool.QueryRowContext(ctx, `
		INSERT INTO wiki_pages (book_id, slug, title, content, created_by, updated_by)
		VALUES ($1::uuid, $2, 'Media Page', '', $3, $3) RETURNING id::text`,
		bookID, fmt.Sprintf("mp-%d", time.Now().UnixNano()%1_000_000_000), user).Scan(&pageID); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	return pageID
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSaveImage_RoundTripAndThumb(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	page := seedPage(t, s, "media-u1")

	// Small image: stored, no thumbnail.
	small, err := s.SaveImage(ctx, page, "media-u1", "tiny.png", testPNG(t, 8, 8))
	if err != nil {
		t.Fatalf("SaveImage small: %v", err)
	}
	if small.Width != 8 || small.Height != 8 || small.ContentType != "image/png" {
		t.Errorf("small meta = %dx%d %s", small.Width, small.Height, small.ContentType)
	}
	if small.ThumbKey != "" {
		t.Errorf("small image grew a thumbnail: %s", small.ThumbKey)
	}

	// Large image: thumbnail generated, openable as jpeg.
	big, err := s.SaveImage(ctx, page, "media-u1", "big.png", testPNG(t, 900, 600))
	if err != nil {
		t.Fatalf("SaveImage big: %v", err)
	}
	if big.ThumbKey == "" {
		t.Fatal("big image has no thumbnail")
	}
	f, ct, err := s.Open(big, true)
	if err != nil {
		t.Fatalf("Open thumb: %v", err)
	}
	defer f.Close()
	if ct != "image/jpeg" {
		t.Errorf("thumb content type = %s", ct)
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width != 400 {
		t.Errorf("thumb decode: %v, width=%d (want 400)", err, cfg.Width)
	}

	// Get joins the book for authz.
	got, err := s.Get(ctx, big.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BookID == "" {
		t.Error("Get did not join book id")
	}
}

func TestSaveImage_RejectsNonImages(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	page := seedPage(t, s, "media-u2")
	cases := map[string][]byte{
		"text":     []byte("hello world"),
		"svg":      []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"sniffing": append([]byte("GIF89a-but-not-really"), bytes.Repeat([]byte{0}, 64)...),
	}
	for name, data := range cases {
		if _, err := s.SaveImage(ctx, page, "media-u2", name, data); err == nil {
			t.Errorf("%s: accepted as an image", name)
		}
	}
	// Oversize: bigger than the 5MB test cap.
	if _, err := s.SaveImage(ctx, page, "media-u2", "huge.bin", make([]byte, 6<<20)); err == nil {
		t.Error("oversize accepted")
	}
}

func TestSweepOrphans(t *testing.T) {
	s := storeForTest(t)
	ctx := context.Background()
	page := seedPage(t, s, "media-u3")

	kept, err := s.SaveImage(ctx, page, "media-u3", "keep.png", testPNG(t, 8, 8))
	if err != nil {
		t.Fatal(err)
	}
	// An orphan: bytes on disk, no row. Backdate so minAge passes.
	orphan := filepath.Join(s.root, "orphan.png")
	if err := os.WriteFile(orphan, testPNG(t, 8, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	// A FRESH orphan must survive (in-flight upload protection).
	fresh := filepath.Join(s.root, "fresh.png")
	if err := os.WriteFile(fresh, testPNG(t, 8, 8), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := s.SweepOrphans(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("aged orphan survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file was swept")
	}
	if _, err := os.Stat(filepath.Join(s.root, kept.ObjectKey)); err != nil {
		t.Error("referenced file was swept")
	}
}
