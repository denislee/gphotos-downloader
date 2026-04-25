package main

type Config struct {
	UserDataDir string
	OutputDir   string
	// StateFile holds the IDs of photos that have already been downloaded
	// in a previous run, so we can skip them. One ID per line.
	StateFile string
	// BatchSize is how many photos to select before each download. Google
	// caps each bulk download at a few hundred items, so going too high
	// means Google silently truncates the zip.
	BatchSize int
	// TrashAfter, when true, moves each batch's photos to Google's trash
	// after the zip has been captured to disk. Useful when the library is
	// large and the virtualized grid keeps re-showing already-downloaded
	// photos at the top — trashing makes the next batch's photos slide
	// into view. Trashed photos are recoverable for 60 days.
	TrashAfter bool
	// LossyTrash relaxes the "extracted media count must match selection"
	// safety gate. In normal mode, a mismatch (zip had fewer photos than
	// were selected on the page) errors out so we don't trash photos that
	// never made it to disk. In lossy mode we forfeit that safety: trash
	// whatever Google considers selected, mark only the count of extracted
	// IDs as "done" (best-effort, by selection order), and continue. Use
	// this when click drops or Google-side grouping prevents progress and
	// you accept that some selected photos may be trashed without a local
	// copy. Trashed items are recoverable from Google's trash for 60 days.
	LossyTrash bool
	// NoScroll skips the virtualized-grid lazy-load step in SelectBatch.
	// We still process whatever is in the DOM after the initial scroll-to-
	// top, but we don't scroll for more. Works best paired with -trash:
	// each batch is bounded by the viewport, the visible items get
	// trashed, and the next batch's waitForGrid resets the view to a
	// fresh top — so progress comes from trashing, not from scrolling.
	// Without -trash this exits quickly once the visible set is done.
	NoScroll bool
	// NoMultipartWait, when true, returns from watchDownloads as soon as
	// the first zip stabilizes instead of holding open a 30s window for
	// additional `Photos-N-002.zip` parts. Use this when batches reliably
	// produce a single zip — it avoids the post-download dead air. If
	// Google does split the download for that batch, the second part will
	// be missed.
	NoMultipartWait bool
	// Zoom applies a CSS `zoom` factor to photos.google.com after login,
	// shrinking the page so more thumbnails fit in the viewport per scroll
	// (handy with -no-scroll, where each batch is bounded by what's
	// visible). 0 or 1 = no zoom; e.g. 0.35 = render at 35% size.
	Zoom float64
}
