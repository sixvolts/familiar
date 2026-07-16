// Package media implements MEDIA-DIAGRAMS Phase 1: images attached
// to wiki/notes pages. Bytes live as flat files under a configured
// directory (the same local-first shape as the skills library);
// Postgres rows (page_media) are the queryable index and the authz
// anchor — page → book → membership. The storage surface is small
// and deliberate so an S3-compatible driver (SeaweedFS et al.) can
// slot in later without touching handlers.
//
// Format policy: png / jpeg / gif / webp, identified by SNIFFING the
// bytes — the client's declared content type is ignored. SVG is
// rejected outright: it's a script container, and serving stored
// user SVGs is an XSS foot-gun no thumbnail is worth.
package media

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register decoders for sniffing + thumbnails
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decode-only webp support

	"github.com/familiar/gateway/internal/db"
)

// thumbWidth is the long-edge cap for generated thumbnails. Images
// already at or under it get no thumbnail — the original serves.
const thumbWidth = 400

// maxDimension rejects header-claimed dimensions beyond anything a
// real photo produces — 12k×12k RGBA is already ~575MB decoded.
const maxDimension = 12000

var ErrNotFound = errors.New("media: not found")
var ErrUnsupported = errors.New("media: unsupported image format (png, jpeg, gif, webp)")

// Media is one page_media row.
type Media struct {
	ID          string    `json:"id"`
	PageID      string    `json:"page_id"`
	UserID      string    `json:"user_id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	ObjectKey   string    `json:"object_key"`
	ThumbKey    string    `json:"thumb_key,omitempty"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	AltText     string    `json:"alt_text,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// BookID is joined from wiki_pages for authz; not a column.
	BookID string `json:"-"`
}

type Store struct {
	pool     *db.Pool
	root     string
	maxBytes int64
}

func NewStore(pool *db.Pool, root string, maxBytes int64) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("media: nil pool")
	}
	if root == "" {
		return nil, fmt.Errorf("media: empty root dir")
	}
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("media: create root: %w", err)
	}
	return &Store{pool: pool, root: root, maxBytes: maxBytes}, nil
}

func (s *Store) MaxBytes() int64 { return s.maxBytes }

var formatMeta = map[string]struct {
	ext string
	ct  string
}{
	"png":  {".png", "image/png"},
	"jpeg": {".jpg", "image/jpeg"},
	"gif":  {".gif", "image/gif"},
	"webp": {".webp", "image/webp"},
}

// SaveImage validates, persists, and indexes one upload. The format
// comes from decoding the bytes, never from the declared type.
func (s *Store) SaveImage(ctx context.Context, pageID, userID, filename string, data []byte) (*Media, error) {
	if int64(len(data)) > s.maxBytes {
		return nil, fmt.Errorf("media: file exceeds the %dMB limit", s.maxBytes>>20)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ErrUnsupported
	}
	meta, ok := formatMeta[format]
	if !ok {
		return nil, ErrUnsupported
	}
	// A "valid" header with degenerate or absurd dimensions is not an
	// image anyone meant to upload. DecodeConfig reads ONLY the
	// header, so claimed-huge dimensions are also the decompression-
	// bomb vector for the thumbnail decode below — cap them.
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > maxDimension || cfg.Height > maxDimension {
		return nil, ErrUnsupported
	}

	key := uuid.NewString() + meta.ext
	if err := os.WriteFile(filepath.Join(s.root, key), data, 0o644); err != nil {
		return nil, fmt.Errorf("media: write: %w", err)
	}

	// Thumbnail: best-effort. A failed decode (corrupt tail, exotic
	// subformat) downgrades to "no thumbnail", never a failed upload.
	thumbKey := ""
	if cfg.Width > thumbWidth {
		if tk, err := s.writeThumb(key, data, cfg.Width); err == nil {
			thumbKey = tk
		}
	}

	row := s.pool.QueryRowContext(ctx, `
		INSERT INTO page_media (
		    page_id, user_id, filename, content_type, size_bytes,
		    object_key, thumb_key, width, height, alt_text
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id::text, created_at`,
		pageID, userID, filename, meta.ct, int64(len(data)),
		key, thumbKey, cfg.Width, cfg.Height, filename)
	m := &Media{
		PageID: pageID, UserID: userID, Filename: filename,
		ContentType: meta.ct, SizeBytes: int64(len(data)),
		ObjectKey: key, ThumbKey: thumbKey,
		Width: cfg.Width, Height: cfg.Height, AltText: filename,
	}
	if err := row.Scan(&m.ID, &m.CreatedAt); err != nil {
		// Index failed — don't leave the bytes orphaned for a day.
		_ = os.Remove(filepath.Join(s.root, key))
		if thumbKey != "" {
			_ = os.Remove(filepath.Join(s.root, thumbKey))
		}
		return nil, fmt.Errorf("media: insert: %w", err)
	}
	return m, nil
}

func (s *Store) writeThumb(key string, data []byte, srcWidth int) (string, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	b := img.Bounds()
	h := int(float64(b.Dy()) * float64(thumbWidth) / float64(b.Dx()))
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, thumbWidth, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	thumbKey := strings.TrimSuffix(key, filepath.Ext(key)) + "_thumb.jpg"
	f, err := os.Create(filepath.Join(s.root, thumbKey))
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := jpeg.Encode(f, dst, &jpeg.Options{Quality: 80}); err != nil {
		_ = os.Remove(filepath.Join(s.root, thumbKey))
		return "", err
	}
	return thumbKey, nil
}

// Get loads one row plus the owning book id (joined for authz).
func (s *Store) Get(ctx context.Context, id string) (*Media, error) {
	var m Media
	err := s.pool.QueryRowContext(ctx, `
		SELECT pm.id::text, pm.page_id::text, pm.user_id, pm.filename,
		       pm.content_type, pm.size_bytes, pm.object_key, pm.thumb_key,
		       pm.width, pm.height, pm.alt_text, pm.created_at,
		       wp.book_id::text
		  FROM page_media pm
		  JOIN wiki_pages wp ON wp.id = pm.page_id
		 WHERE pm.id = $1::uuid
		   AND wp.deleted_at IS NULL`, id).Scan(
		&m.ID, &m.PageID, &m.UserID, &m.Filename,
		&m.ContentType, &m.SizeBytes, &m.ObjectKey, &m.ThumbKey,
		&m.Width, &m.Height, &m.AltText, &m.CreatedAt, &m.BookID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("media: get: %w", err)
	}
	return &m, nil
}

// Open returns the file for serving — the thumbnail when requested
// and present, the original otherwise. Callers own Close.
func (s *Store) Open(m *Media, thumb bool) (*os.File, string, error) {
	key, ct := m.ObjectKey, m.ContentType
	if thumb && m.ThumbKey != "" {
		key, ct = m.ThumbKey, "image/jpeg"
	}
	f, err := os.Open(filepath.Join(s.root, key))
	if err != nil {
		return nil, "", fmt.Errorf("media: open %s: %w", key, err)
	}
	return f, ct, nil
}

// SweepOrphans removes files in the root that no page_media row
// references (page deletes CASCADE the rows; the bytes land here).
// minAge protects in-flight uploads. Returns the number removed.
func (s *Store) SweepOrphans(ctx context.Context, minAge time.Duration) (int, error) {
	rows, err := s.pool.QueryContext(ctx,
		`SELECT object_key, thumb_key FROM page_media`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var ok, tk string
		if err := rows.Scan(&ok, &tk); err != nil {
			return 0, err
		}
		live[ok] = true
		if tk != "" {
			live[tk] = true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0, err
	}
	removed := 0
	cutoff := time.Now().Add(-minAge)
	for _, e := range entries {
		if e.IsDir() || live[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.root, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// ListForPage returns a page's media rows (no book join — callers
// already hold the page).
func (s *Store) ListForPage(ctx context.Context, pageID string) ([]*Media, error) {
	rows, err := s.pool.QueryContext(ctx, `
		SELECT id::text, page_id::text, user_id, filename, content_type,
		       size_bytes, object_key, thumb_key, width, height, alt_text, created_at
		  FROM page_media WHERE page_id = $1::uuid ORDER BY created_at`, pageID)
	if err != nil {
		return nil, fmt.Errorf("media: list: %w", err)
	}
	defer rows.Close()
	out := []*Media{}
	for rows.Next() {
		var m Media
		if err := rows.Scan(&m.ID, &m.PageID, &m.UserID, &m.Filename, &m.ContentType,
			&m.SizeBytes, &m.ObjectKey, &m.ThumbKey, &m.Width, &m.Height, &m.AltText, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// Delete removes the row and its files. Idempotent on the files.
func (s *Store) Delete(ctx context.Context, id string) error {
	var objectKey, thumbKey string
	err := s.pool.QueryRowContext(ctx, `
		DELETE FROM page_media WHERE id = $1::uuid
		RETURNING object_key, thumb_key`, id).Scan(&objectKey, &thumbKey)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("media: delete: %w", err)
	}
	_ = os.Remove(filepath.Join(s.root, objectKey))
	if thumbKey != "" {
		_ = os.Remove(filepath.Join(s.root, thumbKey))
	}
	return nil
}

// FindByPageAndFilename returns the newest row matching (page,
// filename) or ErrNotFound. The share renderer uses it to find
// pre-rendered diagram PNGs (filename carries the content hash).
func (s *Store) FindByPageAndFilename(ctx context.Context, pageID, filename string) (*Media, error) {
	var m Media
	err := s.pool.QueryRowContext(ctx, `
		SELECT id::text, page_id::text, user_id, filename, content_type,
		       size_bytes, object_key, thumb_key, width, height, alt_text, created_at
		  FROM page_media
		 WHERE page_id = $1::uuid AND filename = $2
		 ORDER BY created_at DESC LIMIT 1`, pageID, filename).Scan(
		&m.ID, &m.PageID, &m.UserID, &m.Filename, &m.ContentType,
		&m.SizeBytes, &m.ObjectKey, &m.ThumbKey, &m.Width, &m.Height, &m.AltText, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("media: find: %w", err)
	}
	return &m, nil
}
