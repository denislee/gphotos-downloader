package main

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// mediaExts is the set of file extensions we treat as actual photos/videos
// when verifying zip contents. Google Takeout zips typically pair each media
// file with a same-named `.json` sidecar of metadata; only the media files
// count toward the "did the photos arrive?" check.
var mediaExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".heic": true, ".heif": true, ".webp": true, ".tiff": true, ".tif": true,
	".bmp": true, ".raw": true, ".dng": true, ".cr2": true, ".nef": true,
	".mp4": true, ".mov": true, ".m4v": true, ".webm": true,
	".avi": true, ".3gp": true, ".mkv": true,
}

// ExtractedMedia is one media file written to disk by extractZip. Path is
// the absolute path under the per-zip output folder, Name is its base name
// (the original filename Google preserved in the zip).
type ExtractedMedia struct {
	Path string
	Name string
}

// extractZip unpacks zipPath into a sibling folder named after the zip
// (without the `.zip` suffix), returning the list of media files written
// (the actual files on disk — these are the source of truth for "what
// did we successfully download", not just a count) and the total number
// of zip entries seen. Any path that would escape the target folder
// (zip-slip) is rejected.
//
// Before writing anything to disk, verifyZip is called to walk every
// entry's deflate stream end-to-end. A truncated or corrupt zip surfaces
// as an error here rather than as a half-extracted folder we'd then trash
// the originals against.
func extractZip(zipPath, destBaseDir string) (media []ExtractedMedia, totalEntries int, err error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", zipPath, err)
	}
	defer r.Close()

	if len(r.File) == 0 {
		return nil, 0, fmt.Errorf("zip %s is empty", zipPath)
	}

	if err := verifyZip(&r.Reader, zipPath); err != nil {
		return nil, 0, err
	}

	base := strings.TrimSuffix(filepath.Base(zipPath), ".zip")
	outDir := uniqueDir(filepath.Join(destBaseDir, base))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, 0, err
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return nil, 0, err
	}

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		totalEntries++

		// Defend against zip-slip: the resolved target must stay inside outDir.
		target := filepath.Join(outDir, f.Name)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return media, totalEntries, err
		}
		if !strings.HasPrefix(absTarget, absOut+string(os.PathSeparator)) && absTarget != absOut {
			return media, totalEntries, fmt.Errorf("zip entry %q would escape %s", f.Name, outDir)
		}

		if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
			return media, totalEntries, err
		}

		// Avoid collisions: a Google Takeout zip can list two entries
		// with the same base name (e.g. two `IMG_1234.jpg` from
		// different cameras) and the second would overwrite the first.
		// On extraction, if the target already exists, append `(N)`
		// before the extension until the name is free. Same applies if
		// the user re-extracts a zip into a folder that already has
		// content from a prior run.
		writeTo := uniquePath(absTarget)

		if err := writeZipEntry(f, writeTo); err != nil {
			return media, totalEntries, fmt.Errorf("extract %s: %w", f.Name, err)
		}

		if mediaExts[strings.ToLower(filepath.Ext(writeTo))] {
			media = append(media, ExtractedMedia{
				Path: writeTo,
				Name: filepath.Base(writeTo),
			})
		}
	}
	return media, totalEntries, nil
}

// uniqueDir returns `path` if nothing exists at it, otherwise `path` with
// a short random hex suffix (`path-a3f9b2`) that doesn't yet exist. Used
// to keep each extraction in its own folder even when a prior run already
// produced one with the same name. The suffix is space-free so the path
// stays shell-friendly.
func uniqueDir(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	for {
		var b [3]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(fmt.Errorf("uniqueDir: rand.Read: %w", err))
		}
		cand := fmt.Sprintf("%s-%s", path, hex.EncodeToString(b[:]))
		if _, err := os.Stat(cand); errors.Is(err, os.ErrNotExist) {
			return cand
		}
	}
}

// uniquePath returns `path` if no file exists at it, otherwise the first
// path of the form `name (N).ext` (N starting at 1) that doesn't exist.
// Note this is best-effort against TOCTOU — fine for a single-process
// extractor, not safe under concurrent writers.
func uniquePath(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(cand); errors.Is(err, os.ErrNotExist) {
			return cand
		}
	}
}

// writeBatchManifest persists what landed on disk for one batch so the
// user can audit it after the fact. The file lists, in order:
//   - the photo IDs we clicked, in click order
//   - the media filenames extracted, in zip order
//
// We can't directly map filenames back to IDs (zip carries no Google ID
// metadata for this account), so the manifest is a paired record rather
// than a cross-reference. It also writes the count so a later script can
// detect mismatches without re-parsing the zip.
func writeBatchManifest(baseDir string, batchNum int, clickedIDs []string, media []ExtractedMedia) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(baseDir, fmt.Sprintf("batch-%03d-manifest.txt", batchNum))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Fprintf(f, "# batch %d\n", batchNum)
	fmt.Fprintf(f, "# clicked: %d   extracted: %d\n", len(clickedIDs), len(media))
	fmt.Fprintln(f, "")
	fmt.Fprintln(f, "[clicked-ids]")
	for i, id := range clickedIDs {
		fmt.Fprintf(f, "%d\t%s\n", i, id)
	}
	fmt.Fprintln(f, "")
	fmt.Fprintln(f, "[extracted-files]")
	for i, mm := range media {
		fmt.Fprintf(f, "%d\t%s\t%s\n", i, mm.Name, mm.Path)
	}
	return nil
}

// verifyZip reads every entry's compressed stream end-to-end so the
// archive's CRC32 checksums (verified by archive/zip on Close of each
// entry reader) are checked before we commit any bytes to disk. A
// truncated zip — common when a download partially completed but the
// watcher returned anyway — fails here with `unexpected EOF` or a CRC
// mismatch, and the caller aborts the batch rather than half-extracting.
func verifyZip(r *zip.Reader, zipPath string) error {
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("verify %s: open entry %s: %w", zipPath, f.Name, err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			rc.Close()
			return fmt.Errorf("verify %s: read entry %s: %w", zipPath, f.Name, err)
		}
		// Closing the entry reader is what triggers archive/zip's CRC32
		// check against the local file header — silently skipping this
		// would let a corrupt zip pass verification.
		if err := rc.Close(); err != nil {
			return fmt.Errorf("verify %s: close entry %s (CRC mismatch?): %w", zipPath, f.Name, err)
		}
	}
	return nil
}

func writeZipEntry(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.Create(target)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, rc); err != nil {
		return err
	}
	if fi, statErr := out.Stat(); statErr == nil && fi.Size() == 0 {
		return errors.New("wrote zero bytes")
	}
	return nil
}
