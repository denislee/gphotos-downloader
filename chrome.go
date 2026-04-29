package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// Scraper drives a (head[ful|less]) Chrome to authenticate against
// photos.google.com, range-select every photo in the grid, and let
// Google's own bulk-download flow do the actual download via Shift+D.
type Scraper struct {
	cfg         Config
	allocCtx    context.Context
	allocCancel context.CancelFunc
	browserCtx  context.Context
	browserCanc context.CancelFunc
	// Logger, if non-nil, receives one-line debug messages from the
	// scraper. The TUI wires this to its log channel so chrome.go's
	// internal state shows up in the on-screen log and the debug.log
	// file. Nil = silent.
	Logger func(string)
}

func (s *Scraper) log(format string, args ...any) {
	if s.Logger == nil {
		return
	}
	s.Logger(fmt.Sprintf(format, args...))
}

func NewScraper(cfg Config, headless bool) *Scraper {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(cfg.UserDataDir),
		chromedp.Flag("headless", headless),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		// Google sign-in bounces through accounts.google.com. With Site
		// Isolation on, that cross-origin nav can swap renderer processes
		// and detach our devtools target — chromedp then surfaces every
		// follow-up action as "context canceled". Disabling site isolation
		// keeps a single renderer for the whole flow.
		chromedp.Flag("disable-features", "IsolateOrigins,site-per-process"),
		chromedp.Flag("disable-site-isolation-trials", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.WindowSize(1400, 900),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCanc := chromedp.NewContext(allocCtx)
	return &Scraper{
		cfg:         cfg,
		allocCtx:    allocCtx,
		allocCancel: allocCancel,
		browserCtx:  browserCtx,
		browserCanc: browserCanc,
	}
}

func (s *Scraper) Close() {
	s.browserCanc()
	s.allocCancel()
}

// EnsureLoggedIn opens photos.google.com and waits up to `timeout` for the
// account chooser/login flow to finish. Returns nil once the user is back on
// photos.google.com with the app shell rendered.
func (s *Scraper) EnsureLoggedIn(timeout time.Duration) error {
	s.log("EnsureLoggedIn: navigating to photos.google.com (timeout=%s)", timeout)
	if err := chromedp.Run(s.browserCtx, chromedp.Navigate("https://photos.google.com/")); err != nil {
		s.log("EnsureLoggedIn: navigate error: %v", err)
		return err
	}

	const check = `(() => {
		if (!/(^|\.)photos\.google\.com$/.test(location.host)) return false;
		if (document.querySelector('a[href*="/photo/"]')) return true;
		const main = document.querySelector('div[role="main"], c-wiz[jsrenderer]');
		if (main && document.body && document.body.innerText.length > 200) return true;
		return false;
	})()`

	const stateJS = `(() => ({
		Host: location.host,
		Path: location.pathname,
		HasPhotoLink: !!document.querySelector('a[href*="/photo/"]'),
		HasMain: !!document.querySelector('div[role="main"], c-wiz[jsrenderer]'),
		BodyLen: (document.body ? document.body.innerText.length : 0),
	}))()`

	deadline := time.Now().Add(timeout)
	iter := 0
	for {
		iter++
		if err := s.browserCtx.Err(); err != nil {
			s.log("EnsureLoggedIn: browser context died: %v", err)
			return fmt.Errorf("browser context died: %w", err)
		}
		var ok bool
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(check, &ok)); err == nil && ok {
			s.log("EnsureLoggedIn: signed in after %d polls", iter)
			return nil
		}
		// Periodic state dump so the user can see what the page looks
		// like while we wait — a stuck login otherwise gives no signal.
		if iter%5 == 1 {
			var st struct {
				Host, Path                 string
				HasPhotoLink, HasMain      bool
				BodyLen                    int
			}
			_ = chromedp.Run(s.browserCtx, chromedp.Evaluate(stateJS, &st))
			s.log("EnsureLoggedIn: poll %d host=%s path=%s photoLink=%v main=%v bodyLen=%d",
				iter, st.Host, st.Path, st.HasPhotoLink, st.HasMain, st.BodyLen)
		}
		if time.Now().After(deadline) {
			s.log("EnsureLoggedIn: timed out after %d polls", iter)
			return errors.New("login wait timed out")
		}
		time.Sleep(2 * time.Second)
	}
}

// photoInfo is the per-thumbnail data we read from the DOM in one shot so we
// can drive the mouse without round-tripping for each rect.
type photoInfo struct {
	ID         string
	X, Y, W, H float64
}

// SelectBatch clicks the hover-checkmark of up to `n` photos that are not in
// `done`, scrolling the grid forward as it goes to load more thumbnails. It
// returns the IDs of the photos it actually selected — fewer than `n` means
// the library is exhausted.
//
// We click each checkmark individually rather than range-selecting, because
// Google Photos lazy-unloads thumbnails far above the viewport — a shift+click
// range from the first to the Nth photo only works when both ends are still
// in the DOM, which breaks for batch sizes larger than a viewport or two.
//
// For the *first* photo of a batch we hover to make the checkmark fade in and
// then locate it via the DOM (an element with role="checkbox" or
// aria-label="Select…"); we click its real center rather than guessing a
// pixel offset. After that first click, the grid stays in select-mode and
// every checkmark is permanently visible — subsequent clicks reuse the
// per-thumbnail check element directly without re-hovering.
// waitForGrid scrolls the photos grid to the top and polls until at least
// one thumbnail link is rendered (or `timeout` passes). Google Photos
// restores the prior scroll position on reload, which puts the grid in the
// middle of the library on a fresh session — and Google's virtualized
// list only renders thumbnails near the current viewport, so a SelectBatch
// scan from the top would otherwise see zero photos until the grid
// scrolls into them on its own.
func (s *Scraper) waitForGrid(timeout time.Duration) error {
	s.log("waitForGrid: scrolling to top, waiting up to %s for thumbnails", timeout)
	const scrollTopJS = `(() => {
		window.scrollTo(0, 0);
		if (document.scrollingElement) document.scrollingElement.scrollTo(0, 0);
		document.querySelectorAll('[role="main"]').forEach(el => { el.scrollTop = 0; });
	})()`
	if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(scrollTopJS, nil)); err != nil {
		s.log("waitForGrid: scroll-top failed: %v", err)
		return err
	}

	const countJS = `document.querySelectorAll('a[href*="/photo/"]').length`
	deadline := time.Now().Add(timeout)
	iter := 0
	for {
		iter++
		if err := s.browserCtx.Err(); err != nil {
			s.log("waitForGrid: browser context died: %v", err)
			return fmt.Errorf("browser context died: %w", err)
		}
		var count int
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(countJS, &count)); err != nil {
			s.log("waitForGrid: countJS failed: %v", err)
			return err
		}
		if iter%4 == 1 {
			s.log("waitForGrid: poll %d sees %d photo links", iter, count)
		}
		if count > 0 {
			s.log("waitForGrid: %d photo links rendered after %d polls", count, iter)
			// One more pass to nudge the grid to the top — sometimes the
			// initial scroll lands before the framework has wired up its
			// scroll container, so the second call is the one that takes.
			_ = chromedp.Run(s.browserCtx, chromedp.Evaluate(scrollTopJS, nil))
			time.Sleep(300 * time.Millisecond)
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("photo grid never rendered any thumbnails — " +
				"the page may not have finished loading, or your account has no photos at all")
		}
		// Re-issue scroll-top in case the grid framework has just mounted
		// and is about to restore the previous scroll position; we want
		// to win that race.
		_ = chromedp.Run(s.browserCtx, chromedp.Evaluate(scrollTopJS, nil))
		time.Sleep(500 * time.Millisecond)
	}
}

// SelectBatchResult is the outcome of a SelectBatch call. Exposed so the
// caller can distinguish "library is genuinely empty" from "every visible
// photo was already in the done set" — the two cases need different user
// guidance.
type SelectBatchResult struct {
	IDs           []string // photos we attempted to select (one toggle click per ID)
	TotalSeen     int      // distinct photo IDs we saw across the whole scroll
	SkippedDone   int      // distinct photo IDs we saw that were in `done`
	HitGridBottom bool     // we scrolled and Google stopped loading more
	// ToolbarCount is the number of items the selection toolbar reports as
	// selected after our last click ("N selected"). This is the ground
	// truth — clicks that didn't actually toggle selection don't show up
	// here, and Google may also dedupe live-photo pairs / burst groups.
	// -1 means the count couldn't be parsed from the page.
	ToolbarCount int
}

func (s *Scraper) SelectBatch(done map[string]bool, n int, progress func(selected int)) (SelectBatchResult, error) {
	s.log("SelectBatch: target=%d done-set-size=%d noScroll=%v", n, len(done), s.cfg.NoScroll)
	res := SelectBatchResult{IDs: make([]string, 0, n)}
	selectedSet := make(map[string]bool, n)
	allSeen := make(map[string]bool)
	idleScrolls := 0
	verifiedSelectMode := false

	// Google Photos restores the previous scroll position when the page
	// reloads — so on a fresh launch the grid often opens mid-library, not
	// at the top. Force-scroll to the top, then wait for thumbnails to
	// actually render before scanning, otherwise we'd see zero photos and
	// give up before the grid finished loading.
	if err := s.waitForGrid(20 * time.Second); err != nil {
		s.log("SelectBatch: waitForGrid failed: %v", err)
		return res, err
	}

	for len(res.IDs) < n {
		if err := s.browserCtx.Err(); err != nil {
			return res, err
		}

		var infos []photoInfo
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(`
			(() => Array.from(document.querySelectorAll('a[href*="/photo/"]')).map(a => {
				const m = a.href.match(/\/photo\/([^/?]+)/);
				const r = a.getBoundingClientRect();
				return {ID: m ? m[1] : a.href, X: r.left, Y: r.top, W: r.width, H: r.height};
			}).filter(p => p.W > 0))()
		`, &infos)); err != nil {
			return res, err
		}

		newPhotosThisScan := 0
		for _, p := range infos {
			if !allSeen[p.ID] {
				allSeen[p.ID] = true
				newPhotosThisScan++
				res.TotalSeen++
				if done[p.ID] {
					res.SkippedDone++
				}
			}
		}
		s.log("SelectBatch: scan saw %d photo links (%d new this pass; %d total seen, %d skipped done, %d already selected)",
			len(infos), newPhotosThisScan, res.TotalSeen, res.SkippedDone, len(selectedSet))

		progressed := false
		for _, p := range infos {
			if done[p.ID] || selectedSet[p.ID] {
				continue
			}
			if err := s.toggleSelect(p, !verifiedSelectMode); err != nil {
				s.log("SelectBatch: toggleSelect(%s) error: %v", p.ID, err)
				return res, fmt.Errorf("select %s: %w", p.ID, err)
			}
			// After the very first click, confirm that Google actually
			// entered selection mode. If not, our click didn't land on the
			// checkmark — bail with a clear error rather than silently
			// "selecting" 200 photos that aren't actually selected.
			if !verifiedSelectMode {
				s.log("SelectBatch: verifying first click on %s entered selection mode", p.ID)
				inMode, err := s.waitForSelectionMode(2500 * time.Millisecond)
				if err != nil {
					return res, fmt.Errorf("verify selection: %w", err)
				}
				if !inMode {
					s.log("SelectBatch: first click on %s did NOT enter select mode — trying focus+x fallback", p.ID)
					if err := s.focusAndPressX(p.ID); err != nil {
						return res, fmt.Errorf("first-click fallback for %s: %w", p.ID, err)
					}
					inMode, err = s.waitForSelectionMode(2500 * time.Millisecond)
					if err != nil {
						return res, fmt.Errorf("verify selection: %w", err)
					}
					s.log("SelectBatch: focus+x fallback result inMode=%v", inMode)
				}
				if !inMode {
					diag := s.diagnoseClickTarget(p)
					s.log("SelectBatch: ABORT — first click on %s never entered selection mode — diag: %s", p.ID, diag)
					return res, fmt.Errorf(
						"first click on photo %s did not enter selection mode — diag: %s",
						p.ID, diag)
				}
				s.log("SelectBatch: first click verified — selection mode active")
				verifiedSelectMode = true
			}
			res.IDs = append(res.IDs, p.ID)
			selectedSet[p.ID] = true
			progressed = true
			if progress != nil {
				progress(len(res.IDs))
			}
			time.Sleep(60 * time.Millisecond)
			if len(res.IDs) >= n {
				break
			}
		}

		if len(res.IDs) >= n {
			break
		}

		// In no-scroll mode the batch is bounded by the viewport: once
		// we've processed everything currently in the DOM, we stop. The
		// caller (typically with -trash) is expected to clear the
		// viewport between batches by trashing what we just selected.
		if s.cfg.NoScroll {
			s.log("SelectBatch: no-scroll mode — stopping after viewport with %d selected", len(res.IDs))
			res.HitGridBottom = true
			break
		}

		// Try to load more photos from the virtualized grid.
		loaded, err := s.scrollForMore()
		if err != nil {
			s.log("SelectBatch: scrollForMore error: %v", err)
			return res, err
		}

		// "Idle" means: we didn't select anything, scrollForMore didn't
		// detect growth in its 3s polling window, AND no new photo IDs
		// appeared on this scan. The third clause matters because Google
		// sometimes loads slowly enough that scrollForMore times out, but
		// the next scan still finds new photos — those just happen to all
		// be in the done set (so progressed=false too).
		if !progressed && !loaded && newPhotosThisScan == 0 {
			idleScrolls++
			s.log("SelectBatch: idle pass %d/3 (progressed=%v loaded=%v newScan=%d)",
				idleScrolls, progressed, loaded, newPhotosThisScan)
			if idleScrolls >= 3 {
				s.log("SelectBatch: 3 idle passes — declaring grid bottom hit")
				res.HitGridBottom = true
				break // truly exhausted
			}
		} else {
			idleScrolls = 0
		}
	}

	// Read the toolbar's "N selected" indicator as the ground truth for
	// how many items Google considers selected. If our click count differs
	// from this, some clicks didn't actually toggle selection.
	if count, err := s.selectionCount(); err == nil {
		res.ToolbarCount = count
	} else {
		s.log("SelectBatch: selectionCount error: %v", err)
		res.ToolbarCount = -1
	}
	s.log("SelectBatch: done — clicked=%d toolbar=%d totalSeen=%d skippedDone=%d hitBottom=%v",
		len(res.IDs), res.ToolbarCount, res.TotalSeen, res.SkippedDone, res.HitGridBottom)
	return res, nil
}

// selectionCount returns the number of items the selection toolbar reports
// as currently selected. Returns -1 if no count could be parsed (e.g. the
// toolbar isn't visible, or its locale isn't covered by the patterns).
func (s *Scraper) selectionCount() (int, error) {
	const js = `(() => {
		const isVisible = (el) => {
			const r = el.getBoundingClientRect();
			return r.width > 0 && r.height > 0 && r.top < 200;
		};
		// "selected" and its translations across the locales we try to
		// support. Each is a pattern fragment; the leading regex is built
		// against numbers paired with these.
		const word = '(selected|selecionad[oa]s?|seleccionad[oa]s?|ausgewählt|sélectionnée?s?|selectionnée?s?|selezionat[ie])';
		// "47 selecionados" / "Selected 47" — search anywhere in the
		// element's text rather than requiring start-of-string, since the
		// toolbar prefixes the count with icons and structural elements.
		const numFirst = new RegExp('(?:^|\\s|\\b)(\\d+)\\s+' + word + '\\b', 'i');
		const wordFirst = new RegExp('\\b' + word + '\\s*:?\\s*(\\d+)\\b', 'i');
		const checkText = (text) => {
			const t = (text || '').trim();
			if (!t) return -1;
			let m = t.match(numFirst);
			if (m) return parseInt(m[1], 10);
			m = t.match(wordFirst);
			if (m) return parseInt(m[m.length - 1], 10);
			return -1;
		};
		// Tier 1: elements that usually carry the toolbar title.
		const tier1 = document.querySelectorAll(
			'h1, h2, h3, [role="heading"], [aria-live], [aria-label]'
		);
		for (const el of tier1) {
			if (!isVisible(el)) continue;
			for (const text of [el.getAttribute && el.getAttribute('aria-label'),
								el.innerText, el.textContent]) {
				const n = checkText(text);
				if (n >= 0) return n;
			}
		}
		// Tier 2: any visible element near the very top of the page.
		const tier2 = document.querySelectorAll('div, span, button');
		for (const el of tier2) {
			if (!isVisible(el)) continue;
			const r = el.getBoundingClientRect();
			if (r.top > 150) continue;
			const n = checkText(el.innerText || el.textContent);
			if (n >= 0) return n;
		}
		return -1;
	})()`
	var n int
	err := chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &n))
	return n, err
}

// diagnoseSelectionCount dumps a summary of likely toolbar text so we can
// see why selectionCount returned -1 — usually a locale string we don't
// match yet, or a UI structure where the count isn't where we look.
// Includes any near-top element whose text contains a digit, so the
// "23 selecionados" element shows up even if it's a plain <div>.
func (s *Scraper) diagnoseSelectionCount() string {
	const js = `(() => {
		const isVisible = (el) => {
			const r = el.getBoundingClientRect();
			return r.width > 0 && r.height > 0 && r.top < 200;
		};
		const seen = new Set();
		const out = [];
		// aria-labels near the top.
		document.querySelectorAll('[aria-label]').forEach(el => {
			if (!isVisible(el)) return;
			const al = (el.getAttribute('aria-label') || '').trim();
			if (al && !seen.has('al:' + al)) {
				seen.add('al:' + al);
				out.push('aria=' + al.slice(0, 60));
			}
		});
		// Headers near the top.
		document.querySelectorAll('h1, h2, h3, [role="heading"], [aria-live]').forEach(el => {
			if (!isVisible(el)) return;
			const t = ((el.innerText || el.textContent) || '').trim();
			if (t && !seen.has('h:' + t)) {
				seen.add('h:' + t);
				out.push('hdr=' + t.slice(0, 60));
			}
		});
		// Any near-top div/span/button whose own (not-descendants') text
		// contains a digit — the count element will be a leaf node like
		// <span>23</span> or "23 selecionados".
		const leafTextOf = (el) => {
			let t = '';
			for (const n of el.childNodes) {
				if (n.nodeType === 3) t += n.nodeValue;
			}
			return t.trim();
		};
		document.querySelectorAll('div, span, button').forEach(el => {
			if (!isVisible(el)) return;
			const t = leafTextOf(el);
			if (!t || !/\d/.test(t)) return;
			const key = 'd:' + t;
			if (seen.has(key)) return;
			seen.add(key);
			out.push('txt=' + t.slice(0, 60));
		});
		return out.slice(0, 40).join(' | ');
	})()`
	var s2 string
	_ = chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &s2))
	return s2
}

// SelectByIDs (re)selects the given photo IDs in the grid. Used between
// downloading a batch and trashing it: Google Photos drops the selection
// after the bulk-download click finishes, so the trash button is no longer
// in the toolbar — we have to put the selection back to find it.
//
// Returns the number of IDs we successfully re-selected; if it's less than
// len(ids), some thumbnails couldn't be located in the grid (rare — usually
// means Google has already reshuffled them).
func (s *Scraper) SelectByIDs(ids []string, progress func(selected int)) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	s.log("SelectByIDs: looking for %d IDs in the grid (sample: %v)", len(ids), sampleIDs(ids, 3))
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	selected := make(map[string]bool, len(ids))
	verifiedSelectMode := false
	idleScrolls := 0

	// Start from the top of the grid — the photos we just downloaded are
	// (still) the topmost items, so we'll find them quickly.
	if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(`window.scrollTo(0, 0)`, nil)); err != nil {
		return 0, err
	}
	time.Sleep(500 * time.Millisecond)

	for len(selected) < len(want) {
		if err := s.browserCtx.Err(); err != nil {
			return len(selected), err
		}
		var infos []photoInfo
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(`
			(() => Array.from(document.querySelectorAll('a[href*="/photo/"]')).map(a => {
				const m = a.href.match(/\/photo\/([^/?]+)/);
				const r = a.getBoundingClientRect();
				return {ID: m ? m[1] : a.href, X: r.left, Y: r.top, W: r.width, H: r.height};
			}).filter(p => p.W > 0))()
		`, &infos)); err != nil {
			return len(selected), err
		}

		progressed := false
		for _, p := range infos {
			if !want[p.ID] || selected[p.ID] {
				continue
			}
			if err := s.toggleSelect(p, !verifiedSelectMode); err != nil {
				return len(selected), fmt.Errorf("reselect %s: %w", p.ID, err)
			}
			if !verifiedSelectMode {
				inMode, err := s.waitForSelectionMode(2500 * time.Millisecond)
				if err != nil {
					return len(selected), fmt.Errorf("verify reselection: %w", err)
				}
				if !inMode {
					// Corner click didn't take. Strategy 1 commonly hits an
					// SVG path overlay whose visible geometry doesn't align
					// with the underlying click handler. Try focus+'x' as a
					// fallback: it bypasses the checkmark overlay entirely.
					if err := s.focusAndPressX(p.ID); err != nil {
						return len(selected), fmt.Errorf("reselect %s fallback: %w", p.ID, err)
					}
					inMode, err = s.waitForSelectionMode(2500 * time.Millisecond)
					if err != nil {
						return len(selected), fmt.Errorf("verify reselection: %w", err)
					}
				}
				if !inMode {
					diag := s.diagnoseClickTarget(p)
					return len(selected), fmt.Errorf(
						"first reselect click on photo %s did not enter selection mode — diag: %s",
						p.ID, diag)
				}
				verifiedSelectMode = true
			}
			selected[p.ID] = true
			progressed = true
			if progress != nil {
				progress(len(selected))
			}
			time.Sleep(60 * time.Millisecond)
			if len(selected) >= len(want) {
				break
			}
		}

		if len(selected) >= len(want) {
			break
		}

		loaded, err := s.scrollForMore()
		if err != nil {
			return len(selected), err
		}
		if !progressed && !loaded {
			idleScrolls++
			s.log("SelectByIDs: idle pass %d/3 (selected=%d/%d)", idleScrolls, len(selected), len(want))
			if idleScrolls >= 3 {
				break
			}
		} else {
			idleScrolls = 0
		}
	}
	s.log("SelectByIDs: done — re-selected %d of %d IDs", len(selected), len(want))
	return len(selected), nil
}

// diagnoseToolbar dumps aria-label/title/data-tooltip of every visible
// element near the top of the page. Used in the trash-button-not-found
// error so we can see what's actually rendered when our matcher misses
// (typically a locale we haven't seen).
func (s *Scraper) diagnoseToolbar() string {
	const js = `(() => {
		const out = [];
		const seen = new Set();
		document.querySelectorAll('[aria-label], [data-tooltip], [title]').forEach(el => {
			const r = el.getBoundingClientRect();
			if (r.top > 200 || r.width === 0 || r.height === 0) return;
			const labels = [
				el.getAttribute('aria-label'),
				el.getAttribute('data-tooltip'),
				el.getAttribute('title'),
			].filter(x => x && x.trim());
			for (const l of labels) {
				if (seen.has(l)) continue;
				seen.add(l);
				out.push(l.slice(0, 60));
			}
		});
		return 'top-labels=[' + out.slice(0, 40).join(' | ') + ']';
	})()`
	var s2 string
	_ = chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &s2))
	return s2
}

// SelectionVisible is the exported form of selectionVisible — it lets callers
// check whether the selection toolbar is currently on screen (e.g. to decide
// whether they need to re-select after a download).
func (s *Scraper) SelectionVisible() (bool, error) { return s.selectionVisible() }

// waitForSelectionMode polls selectionVisible until it returns true or the
// timeout elapses. The toolbar can take a beat to render after the click,
// especially after a recent download when the page is animating snackbars
// or repainting the grid — a single 500ms check sometimes fires too early.
func (s *Scraper) waitForSelectionMode(timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := s.selectionVisible()
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// focusAndPressX is strategy 3 from toggleSelect, exposed so the first-click
// verifier can fall back to it explicitly when the corner-click strategy
// landed but didn't actually toggle selection. Google Photos accepts 'x' as
// the documented "toggle selection" shortcut on the focused thumbnail.
func (s *Scraper) focusAndPressX(id string) error {
	var focused bool
	focusJS := fmt.Sprintf(`(() => {
		const a = document.querySelector('a[href*="/photo/%s"]');
		if (!a) return false;
		a.setAttribute('tabindex', '0');
		a.focus();
		return document.activeElement === a;
	})()`, id)
	if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(focusJS, &focused)); err != nil {
		return err
	}
	if !focused {
		return fmt.Errorf("could not focus photo %s", id)
	}
	return chromedp.Run(s.browserCtx, chromedp.KeyEvent("x"))
}

// scrollForMore tries to make Google Photos lazy-load the next chunk of
// thumbnails. Returns true if the photo count grew or the document got
// taller after scrolling.
//
// Google Photos virtualizes the grid (only ~one screenful of thumbnails
// is in the DOM at any time), so a naive `window.scrollBy` past the
// viewport often falls into empty space without triggering the next
// chunk. The most reliable trigger is `scrollIntoView` on the *last*
// currently-loaded photo — the framework then renders the next batch.
func (s *Scraper) scrollForMore() (bool, error) {
	const measure = `(() => ({
		Count: document.querySelectorAll('a[href*="/photo/"]').length,
		Height: document.documentElement.scrollHeight,
	}))()`
	var before struct {
		Count  int
		Height float64
	}
	if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(measure, &before)); err != nil {
		return false, err
	}

	const scrollJS = `(() => {
		const links = document.querySelectorAll('a[href*="/photo/"]');
		if (links.length > 0) {
			links[links.length - 1].scrollIntoView({block: 'end', behavior: 'instant'});
		}
		window.scrollTo(0, document.documentElement.scrollHeight);
		if (document.scrollingElement) {
			document.scrollingElement.scrollTo(0, document.scrollingElement.scrollHeight);
		}
		document.querySelectorAll('[role="main"]').forEach(el => {
			if (el.scrollHeight > el.clientHeight) el.scrollTop = el.scrollHeight;
		});
	})()`
	if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(scrollJS, nil)); err != nil {
		return false, err
	}

	// Poll for the lazy-load to materialize. ~3 s is enough on a normal
	// connection; if Google is slow, we'll just iterate again.
	for i := 0; i < 6; i++ {
		time.Sleep(500 * time.Millisecond)
		var after struct {
			Count  int
			Height float64
		}
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(measure, &after)); err != nil {
			return false, err
		}
		if after.Count > before.Count || after.Height > before.Height+50 {
			s.log("scrollForMore: grew count %d→%d height %.0f→%.0f after %d polls",
				before.Count, after.Count, before.Height, after.Height, i+1)
			return true, nil
		}
	}
	s.log("scrollForMore: no growth after 6 polls (count=%d height=%.0f)", before.Count, before.Height)
	return false, nil
}

// TrashSelection clicks the "Move to trash" button in the selection toolbar
// and confirms the resulting dialog. Used between batches when the caller
// wants to keep the grid clear of already-downloaded photos. The trashed
// photos remain recoverable from Google Photos' Trash for 60 days.
func (s *Scraper) TrashSelection() error {
	s.log("TrashSelection: starting (looking for trash button in toolbar)")
	// Find the selection-toolbar trash button. The matcher is tiered so
	// we don't accidentally hit Google Photos' sidebar "Trash" nav link
	// (label = "Lixeira" / "Papelera" / "Trash"), which would navigate
	// away from the grid and drop the selection. Tiers, in order:
	//
	//   1. Exact aria-label match against the per-locale "Move (items)
	//      to trash" verb. This is the action button — never the nav.
	//   2. Substring on aria-label, but only for verb forms that include
	//      a directional preposition ("mover ... para", "move to"),
	//      which the sidebar nav doesn't.
	//
	// Anchor tags (the sidebar Trash link is an <a href>) are excluded
	// throughout — toolbar actions are buttons, never links.
	const trashButtonJS = `
		const isVisible = (el) => {
			const r = el.getBoundingClientRect();
			return r.width > 0 && r.height > 0;
		};
		const isNavLink = (el) => el.tagName === 'A' || el.closest('a[href]');
		const exactLabels = [
			'Move to trash', 'Move items to trash', 'Move 1 item to trash',
			'Mover para a lixeira', 'Mover itens para a lixeira', 'Mover item para a lixeira',
			'Enviar para a lixeira',
			'Mover a la papelera', 'Mover elementos a la papelera',
			'In den Papierkorb', 'In den Papierkorb verschieben',
			'Elemente in den Papierkorb verschieben',
			'Mettre à la corbeille', 'Déplacer vers la corbeille',
			'Sposta nel cestino', 'Sposta elementi nel cestino',
		];
		for (const l of exactLabels) {
			for (const el of document.querySelectorAll('[aria-label="' + l + '"]')) {
				if (isVisible(el) && !isNavLink(el)) return el;
			}
		}
		// Substring fallback. Require a verb+preposition form so we only
		// match the action ("MOVE TO trash") and never the bare-noun nav
		// ("Trash"/"Lixeira"/"Papelera"). Same on tooltips and titles.
		const verbPhrases = [
			'move to trash', 'move items to trash', 'move item to trash',
			'mover para a lixeira', 'mover itens para a lixeira',
			'mover item para a lixeira', 'enviar para a lixeira',
			'mover a la papelera',
			'in den papierkorb',
			'mettre à la corbeille', 'déplacer vers la corbeille',
			'sposta nel cestino',
		];
		const matchesVerb = (s) => {
			s = (s || '').toLowerCase();
			return s && verbPhrases.some(p => s.includes(p));
		};
		for (const el of document.querySelectorAll('[aria-label], [data-tooltip], [title]')) {
			if (!isVisible(el) || isNavLink(el)) continue;
			if (matchesVerb(el.getAttribute('aria-label')) ||
				matchesVerb(el.getAttribute('data-tooltip')) ||
				matchesVerb(el.getAttribute('title'))) {
				return el;
			}
		}
		return null;
	`
	clicked, err := s.clickElementByJS(trashButtonJS)
	if err != nil {
		s.log("TrashSelection: trash button click error: %v", err)
		return err
	}
	if clicked {
		s.log("TrashSelection: clicked trash button via aria-label match")
	}
	if !clicked {
		s.log("TrashSelection: trash button not found by aria — trying Shift+Delete shortcut")
		// Fallback: the documented Google Photos shortcut for "delete
		// selected" is Shift+Delete. Cheap to try; harmless if nothing
		// is selected (Google ignores it).
		_ = chromedp.Run(s.browserCtx,
			input.DispatchKeyEvent(input.KeyDown).
				WithKey("Delete").WithCode("Delete").WithWindowsVirtualKeyCode(46).
				WithModifiers(input.ModifierShift),
			input.DispatchKeyEvent(input.KeyUp).
				WithKey("Delete").WithCode("Delete").WithWindowsVirtualKeyCode(46).
				WithModifiers(input.ModifierShift),
		)
		time.Sleep(700 * time.Millisecond)
		// If the shortcut worked, the confirm dialog is now open — we'll
		// pick it up below. Otherwise dump diagnostics and bail.
		var dialogOpen bool
		_ = chromedp.Run(s.browserCtx, chromedp.Evaluate(`(() => {
			const d = document.querySelector('[role="dialog"], [role="alertdialog"]');
			if (!d) return false;
			const r = d.getBoundingClientRect();
			return r.width > 0 && r.height > 0;
		})()`, &dialogOpen))
		s.log("TrashSelection: after Shift+Delete dialogOpen=%v", dialogOpen)
		if !dialogOpen {
			s.log("TrashSelection: ABORT — no trash button and no dialog from shortcut. toolbar diag: %s", s.diagnoseToolbar())
			return fmt.Errorf("trash button not found in selection toolbar — diag: %s", s.diagnoseToolbar())
		}
	}

	// Google Photos pops a "Move to trash?" confirm dialog. Detect the
	// dialog *structurally* — a container that has both a visible
	// Cancel button and at least one other button — rather than by
	// `role="dialog"`, because the actual dialog in current Google Photos
	// doesn't always carry that role and a role-based check returns
	// "dialog gone" on first poll, making the loop fall through silently
	// without ever clicking confirm. The non-cancel sibling is the
	// confirm action; that's the click target.
	const confirmJS = `
		const isVisible = (el) => {
			const r = el.getBoundingClientRect();
			return r.width > 0 && r.height > 0;
		};
		const cancelWords = ['cancelar', 'cancel', 'annuler', 'abbrechen', 'annulla'];
		const isCancel = (b) => {
			const t = ((b.innerText || b.textContent || '') + '').trim().toLowerCase();
			return cancelWords.some(w => t === w || t.startsWith(w + ' ') || t.endsWith(' ' + w));
		};
		const allBtns = Array.from(document.querySelectorAll('button, [role="button"]')).filter(isVisible);
		// Locate a visible cancel button. The dialog is the smallest
		// ancestor that also contains a non-cancel button — that
		// non-cancel sibling is what we want to click.
		const cancels = allBtns.filter(isCancel);
		for (const c of cancels) {
			let par = c.parentElement;
			for (let depth = 0; depth < 10 && par; depth++, par = par.parentElement) {
				const innerBtns = Array.from(par.querySelectorAll('button, [role="button"]')).filter(isVisible);
				const others = innerBtns.filter(b => !isCancel(b));
				if (others.length > 0) return others[others.length - 1];
			}
		}
		return null;
	`
	// Match the same structural detection so we don't loop forever when
	// the dialog is gone (or never appeared). "Open" = a visible cancel
	// button paired with a sibling action button.
	const dialogOpenJS = `(() => {
		const isVisible = (el) => {
			const r = el.getBoundingClientRect();
			return r.width > 0 && r.height > 0;
		};
		const cancelWords = ['cancelar', 'cancel', 'annuler', 'abbrechen', 'annulla'];
		const isCancel = (b) => {
			const t = ((b.innerText || b.textContent || '') + '').trim().toLowerCase();
			return cancelWords.some(w => t === w || t.startsWith(w + ' ') || t.endsWith(' ' + w));
		};
		const cancels = Array.from(document.querySelectorAll('button, [role="button"]'))
			.filter(isVisible).filter(isCancel);
		for (const c of cancels) {
			let par = c.parentElement;
			for (let depth = 0; depth < 10 && par; depth++, par = par.parentElement) {
				const inner = Array.from(par.querySelectorAll('button, [role="button"]'))
					.filter(isVisible).filter(b => !isCancel(b));
				if (inner.length > 0) return true;
			}
		}
		return false;
	})()`
	// Phase 1: wait for the confirm dialog to actually appear before we
	// start trying to click it. The previous version started the
	// click-then-check loop immediately, and on the first iteration
	// (~400ms in) the dialog often hadn't rendered yet — `dialogOpenJS`
	// returned false, the loop interpreted that as "dialog already
	// dismissed, success", and we moved on without ever confirming.
	// Wait up to 8s, polling every 200ms.
	dialogDeadline := time.Now().Add(8 * time.Second)
	dialogAppeared := false
	dialogPolls := 0
	for time.Now().Before(dialogDeadline) {
		dialogPolls++
		if err := s.browserCtx.Err(); err != nil {
			return err
		}
		var open bool
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(dialogOpenJS, &open)); err != nil {
			return err
		}
		if open {
			s.log("TrashSelection: confirm dialog detected after %d polls", dialogPolls)
			dialogAppeared = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !dialogAppeared {
		s.log("TrashSelection: confirm dialog never appeared (polled %d times over 8s)", dialogPolls)
		// Two cases land here:
		//   (a) Google didn't show a confirm dialog this time (rare —
		//       has happened on tiny selections in some UI versions).
		//   (b) The trash click didn't actually toggle anything; we
		//       silently move on but nothing got trashed.
		// Both are rare; surface (b) as a soft warning. The caller's
		// state-append already happened upstream so missing trash here
		// just means the next batch will re-encounter the same photos.
		return errors.New("trash confirm dialog never appeared after clicking the toolbar trash button — photos likely not trashed; nothing to confirm")
	}

	// Phase 2: dialog is up. Loop click-then-recheck until it's gone or
	// the browser context is canceled (user quit).
	for attempt := 0; ; attempt++ {
		if err := s.browserCtx.Err(); err != nil {
			return err
		}
		clicked, err := s.clickElementByJS(confirmJS)
		if err != nil {
			s.log("TrashSelection: confirm click error attempt %d: %v", attempt, err)
			return err
		}
		if attempt == 0 || attempt%3 == 0 {
			s.log("TrashSelection: confirm-click attempt %d clicked=%v", attempt, clicked)
		}
		if attempt > 0 && attempt%5 == 0 {
			// Periodic Enter press as a backup pathway in case our
			// querySelector is missing the actual confirm button.
			s.log("TrashSelection: pressing Enter as backup confirm (attempt %d)", attempt)
			_ = chromedp.Run(s.browserCtx, chromedp.KeyEvent("\r"))
		}
		time.Sleep(400 * time.Millisecond)
		var stillOpen bool
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(dialogOpenJS, &stillOpen)); err != nil {
			return err
		}
		if !stillOpen {
			s.log("TrashSelection: confirm dialog dismissed after %d attempts", attempt+1)
			break
		}
	}
	// Give Google a moment to actually delete and refresh the grid.
	time.Sleep(2 * time.Second)
	s.log("TrashSelection: complete")
	return nil
}

// WaitForTrashToastGone waits for Google Photos' trash snackbar to
// disappear. Two consecutive toasts can show up: the in-progress
// "Movendo para a lixeira" / "Moving to trash" while the server-side
// delete runs, and the post-delete confirmation "Movido(s) para a
// lixeira" / "Moved to trash" with an Undo button — while either is
// visible the trash isn't fully committed (Undo can still revert it),
// and reloading or selecting the next batch races that state and can
// leave the grid showing stale thumbnails of items about to vanish.
// Returns nil when no trash toast is visible (or none ever appeared).
// Returns nil on timeout too — we don't want a stuck toast to abort
// the whole run.
func (s *Scraper) WaitForTrashToastGone(timeout time.Duration) error {
	s.log("WaitForTrashToastGone: polling for trash toast (timeout=%s)", timeout)
	const js = `(() => {
		const isVisible = (el) => {
			const r = el.getBoundingClientRect();
			return r.width > 0 && r.height > 0;
		};
		// In-progress: "Movendo para a lixeira" (pt) / "Moving to trash" (en)
		// / "Moviendo a la papelera" (es) / "Déplacement vers la corbeille" (fr)
		// / "In den Papierkorb verschieben" (de) / "Spostamento nel cestino" (it).
		// Post-delete (with Undo button): "Movido(s) para a lixeira" /
		// "Moved to trash" / "Movido a la papelera" / "Déplacé vers la corbeille"
		// / "In den Papierkorb verschoben" / "Spostato nel cestino".
		const rx = /(movendo para a lixeira|movido[s]? para a lixeira|moving to trash|moving items to trash|moved to trash|item[s]? moved to trash|moviendo a la papelera|movido[s]? a la papelera|déplacement vers la corbeille|déplacé[s]? vers la corbeille|verschieb\w* in den papierkorb|in den papierkorb verschoben|spostamento nel cestino|spostando nel cestino|spostat[oi] nel cestino)/i;
		const sels = [
			'[role="alert"]', '[role="status"]',
			'[aria-live="polite"]', '[aria-live="assertive"]',
		];
		const seen = new Set();
		for (const sel of sels) {
			for (const el of document.querySelectorAll(sel)) {
				if (seen.has(el)) continue;
				seen.add(el);
				if (!isVisible(el)) continue;
				const t = ((el.innerText || el.textContent) || '').trim();
				if (!t) continue;
				if (rx.test(t)) return true;
			}
		}
		return false;
	})()`
	deadline := time.Now().Add(timeout)
	// Phase 1: wait briefly for the toast to actually show up. If it never
	// appears (small selection, fast confirm), there's nothing to wait on.
	sawToast := false
	appearDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(appearDeadline) {
		if err := s.browserCtx.Err(); err != nil {
			return err
		}
		var visible bool
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &visible)); err == nil && visible {
			sawToast = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !sawToast {
		s.log("WaitForTrashToastGone: toast never appeared within 3s — skipping wait")
		return nil
	}
	// Phase 2: poll until it's gone or we hit the overall timeout. The
	// in-progress toast and the post-delete confirmation toast can be
	// rendered as separate elements with a brief gap between them; require
	// the "gone" state to hold for ~1s of consecutive polls so we don't
	// return during the swap and start the next batch while the
	// confirmation toast is still about to appear.
	const stableNeeded = 4 // 4 * 300ms ≈ 1.2s
	stableCount := 0
	for {
		if err := s.browserCtx.Err(); err != nil {
			return err
		}
		var visible bool
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &visible)); err == nil {
			if !visible {
				stableCount++
				if stableCount >= stableNeeded {
					s.log("WaitForTrashToastGone: toast dismissed")
					return nil
				}
			} else {
				stableCount = 0
			}
		}
		if time.Now().After(deadline) {
			s.log("WaitForTrashToastGone: timeout after %s — toast still visible, proceeding anyway", timeout)
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// toggleSelect toggles the selection state of photo `p`. Approach:
//
//  1. Hover the thumbnail center so the hover-revealed checkmark renders.
//  2. Use `elementFromPoint` at several near-corner offsets to find the
//     topmost element there. If that element is *not* the photo link or
//     anything inside it (i.e. it's a separate overlay — almost always the
//     checkmark), click its center.
//  3. Fall back to a checkmark element discovered via DOM query, then to
//     focus + 'x', then to a blind corner click.
//
// `withHover` only controls whether step 1's mouse-move is performed; once
// the grid is in select-mode, callers can pass false and skip the 350ms
// hover settle.
func (s *Scraper) toggleSelect(p photoInfo, withHover bool) error {
	if withHover {
		if err := chromedp.Run(s.browserCtx,
			input.DispatchMouseEvent(input.MouseMoved, p.X+p.W/2, p.Y+p.H/2),
		); err != nil {
			return err
		}
		time.Sleep(350 * time.Millisecond)
	}

	// Strategy 1: elementFromPoint at corner offsets after hover. The
	// checkmark overlay sits absolutely-positioned over the thumbnail, so
	// whatever document.elementFromPoint() returns at a corner pixel is
	// the checkmark (or its container) — *unless* it returns the link
	// itself, which means the overlay isn't there yet.
	//
	// Click at the offset point itself (not the element's bbox center):
	// for SVG path checkmarks, the path's bounding-box center can sit
	// outside the photo's corner, and clicking there misses the
	// interactive area entirely.
	pointJS := fmt.Sprintf(`(() => {
		const a = document.querySelector('a[href*="/photo/%s"]');
		if (!a) return null;
		const r = a.getBoundingClientRect();
		for (const [dx, dy] of [[14,14],[18,18],[22,22],[10,10],[26,26]]) {
			const x = r.left + dx, y = r.top + dy;
			const e = document.elementFromPoint(x, y);
			if (e && e !== a && !a.contains(e)) {
				return {X: x, Y: y};
			}
		}
		return null;
	})()`, p.ID)
	var pt struct{ X, Y float64 }
	if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(pointJS, &pt)); err != nil {
		return err
	}
	if pt.X != 0 || pt.Y != 0 {
		s.log("toggleSelect[%s]: strategy=elementFromPoint click=(%.0f,%.0f)", p.ID, pt.X, pt.Y)
		return s.click(pt.X, pt.Y)
	}

	// Strategy 2: discover a checkmark element by aria roles in the
	// photo's ancestry.
	checkJS := fmt.Sprintf(`(() => {
		const a = document.querySelector('a[href*="/photo/%s"]');
		if (!a) return null;
		let el = a;
		for (let i = 0; i < 8 && el; i++, el = el.parentElement) {
			const cands = el.querySelectorAll(
				'[role="checkbox"], [aria-checked], ' +
				'[aria-label*="Select"], [aria-label*="Selecion"], [aria-label*="Seleccion"]'
			);
			for (const c of cands) {
				const r = c.getBoundingClientRect();
				if (r.width > 0 && r.height > 0) {
					return {X: r.left + r.width/2, Y: r.top + r.height/2};
				}
			}
		}
		return null;
	})()`, p.ID)
	if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(checkJS, &pt)); err != nil {
		return err
	}
	if pt.X != 0 || pt.Y != 0 {
		s.log("toggleSelect[%s]: strategy=ariaCheckbox click=(%.0f,%.0f)", p.ID, pt.X, pt.Y)
		return s.click(pt.X, pt.Y)
	}

	// Strategy 3: focus the photo and press 'x' (Google Photos' documented
	// "toggle selection" shortcut).
	var focused bool
	focusJS := fmt.Sprintf(`(() => {
		const a = document.querySelector('a[href*="/photo/%s"]');
		if (!a) return false;
		a.setAttribute('tabindex', '0');
		a.focus();
		return document.activeElement === a;
	})()`, p.ID)
	if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(focusJS, &focused)); err == nil && focused {
		if err := chromedp.Run(s.browserCtx, chromedp.KeyEvent("x")); err == nil {
			s.log("toggleSelect[%s]: strategy=focus+x", p.ID)
			return nil
		}
	}

	// Strategy 4: blind corner click, last resort.
	s.log("toggleSelect[%s]: strategy=blindCorner click=(%.0f,%.0f)", p.ID, p.X+14, p.Y+14)
	return s.click(p.X+14, p.Y+14)
}

// diagnoseClickTarget returns a string describing what's around photo `p`
// after a hover, used in error messages when the verifier reports that
// selection didn't take. Lets us see which element is actually under the
// click positions we tried.
func (s *Scraper) diagnoseClickTarget(p photoInfo) string {
	js := fmt.Sprintf(`(() => {
		const a = document.querySelector('a[href*="/photo/%s"]');
		if (!a) return 'photo not in DOM';
		const r = a.getBoundingClientRect();
		const desc = (e) => e
			? (e.tagName + (e.id?'#'+e.id:'') + '.' + ((e.className||'').toString().slice(0,30))
			   + ' role=' + (e.getAttribute('role')||'-')
			   + ' label=' + ((e.getAttribute('aria-label')||'-').slice(0,30)))
			: 'null';
		const out = [];
		out.push('rect=' + r.left.toFixed(0) + ',' + r.top.toFixed(0) + ' ' + r.width.toFixed(0) + 'x' + r.height.toFixed(0));
		out.push('link=' + desc(a));
		for (const [dx, dy] of [[14,14],[20,20],[r.width/2, r.height/2]]) {
			const x = r.left + dx, y = r.top + dy;
			const e = document.elementFromPoint(x, y);
			out.push('@+' + dx.toFixed(0) + ',' + dy.toFixed(0) + '=' + desc(e));
		}
		let par = a.parentElement;
		for (let i = 0; i < 4 && par; i++, par = par.parentElement) {
			out.push('anc[' + i + ']=' + desc(par));
		}
		return out.join(' | ');
	})()`, p.ID)
	var s2 string
	_ = chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &s2))
	return s2
}

// selectionVisible reports whether the selection toolbar is on screen,
// which is our proxy for "Google considers something selected". The check
// requires an actual *visible* (non-zero rect) element — Google Photos
// pre-renders the toolbar with display:none, so a plain querySelector
// would match even when nothing is selected.
func (s *Scraper) selectionVisible() (bool, error) {
	const js = `(() => {
		const labels = [
			'Clear selection', 'Cancel selection',
			'Limpar seleção', 'Cancelar seleção',
			'Borrar selección', 'Cancelar selección',
			'Auswahl aufheben', 'Auswahl löschen',
			'Effacer la sélection', 'Annuler la sélection',
			'Cancella selezione', 'Annulla selezione',
		];
		const isVisible = (el) => {
			const r = el.getBoundingClientRect();
			return r.width > 0 && r.height > 0;
		};
		for (const l of labels) {
			const el = document.querySelector('[aria-label="' + l + '"]');
			if (el && isVisible(el)) return true;
		}
		const all = document.querySelectorAll('[aria-label]');
		for (const el of all) {
			const al = (el.getAttribute('aria-label') || '').toLowerCase();
			const looksLikeCancel =
				al.includes('clear selection') ||
				al.includes('cancel selection') ||
				al.includes('limpar seleção') || al.includes('cancelar seleção') ||
				al.includes('borrar selección') || al.includes('cancelar selección');
			if (looksLikeCancel && isVisible(el)) return true;
		}
		return false;
	})()`
	var ok bool
	err := chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &ok))
	return ok, err
}

// diagnoseAfterSelect returns a short string describing the page state for
// debugging when selection or download triggering fails. Includes the
// current URL and the aria-labels of *visible* elements in the top 100px
// of the viewport (where the selection toolbar would be).
func (s *Scraper) diagnoseAfterSelect() string {
	const js = `(() => {
		const top = [];
		document.querySelectorAll('[aria-label]').forEach(el => {
			const al = el.getAttribute('aria-label');
			if (!al) return;
			const r = el.getBoundingClientRect();
			if (r.top < 100 && r.bottom > 0 && r.width > 0 && r.height > 0) {
				top.push(al);
			}
		});
		const unique = Array.from(new Set(top)).slice(0, 30);
		return 'url=' + location.href + ' || top-labels=[' + unique.join(' | ') + ']';
	})()`
	var s2 string
	_ = chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &s2))
	return s2
}

// RefreshGrid forces a hard reload of photos.google.com and re-waits for
// the grid to render. Used as a sanity check before concluding the library
// is exhausted: if a batch returned zero selectable photos but a reload
// surfaces fresh thumbnails, the previous run was looking at a stale
// virtualized DOM (often after a long trash sequence where Google hadn't
// fully repaginated) rather than a genuinely empty library.
func (s *Scraper) RefreshGrid(timeout time.Duration) error {
	s.log("RefreshGrid: hard reload of photos.google.com")
	if err := chromedp.Run(s.browserCtx, chromedp.Navigate("https://photos.google.com/")); err != nil {
		s.log("RefreshGrid: navigate error: %v", err)
		return fmt.Errorf("refresh navigate: %w", err)
	}
	// Give the SPA a moment to mount before waitForGrid starts polling;
	// otherwise the first poll can race the framework and read a stale
	// thumbnail count from the previous view.
	time.Sleep(1500 * time.Millisecond)
	return s.waitForGrid(timeout)
}

// VerifyTrashed polls the photos grid DOM for up to `timeout` until none
// of `ids` appear as `a[href*="/photo/<id>"]` links. Returns the slice of
// IDs that are still visible when the timeout expires (empty == fully
// trashed). The check is purely DOM-based: Google Photos removes a photo's
// thumbnail link from the grid almost immediately after a successful trash
// confirm, so if any IDs persist past a few seconds it usually means the
// trash never went through (e.g. confirm dialog was dismissed without
// committing).
func (s *Scraper) VerifyTrashed(ids []string, timeout time.Duration) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Build the JS check once. We pass IDs in as a JSON-encoded literal
	// rather than interpolating to keep escaping safe even for IDs with
	// unusual characters.
	idLits := make([]string, 0, len(ids))
	for _, id := range ids {
		// IDs are alphanumeric in practice; defensively quote.
		idLits = append(idLits, `"`+id+`"`)
	}
	js := fmt.Sprintf(`(() => {
		const ids = [%s];
		const remaining = [];
		for (const id of ids) {
			const a = document.querySelector('a[href*="/photo/' + id + '"]');
			if (a) {
				const r = a.getBoundingClientRect();
				if (r.width > 0 && r.height > 0) remaining.push(id);
			}
		}
		return remaining;
	})()`, strings.Join(idLits, ","))

	s.log("VerifyTrashed: polling for up to %s on %d IDs", timeout, len(ids))
	deadline := time.Now().Add(timeout)
	var remaining []string
	iter := 0
	for {
		iter++
		if err := s.browserCtx.Err(); err != nil {
			return nil, err
		}
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &remaining)); err != nil {
			return nil, err
		}
		if len(remaining) == 0 {
			s.log("VerifyTrashed: all %d IDs gone from grid after %d polls", len(ids), iter)
			return nil, nil
		}
		if time.Now().After(deadline) {
			s.log("VerifyTrashed: timeout — %d of %d IDs still in grid (sample: %v)",
				len(remaining), len(ids), sampleIDs(remaining, 5))
			return remaining, nil
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func sampleIDs(ids []string, n int) []string {
	if len(ids) <= n {
		return ids
	}
	return ids[:n]
}

// ClearSelection sends Escape to drop the current selection so the next batch
// starts clean.
func (s *Scraper) ClearSelection() error {
	s.log("ClearSelection: pressing Escape and scrolling to top")
	if err := chromedp.Run(s.browserCtx,
		input.DispatchKeyEvent(input.KeyDown).
			WithKey("Escape").WithCode("Escape").WithWindowsVirtualKeyCode(27),
		input.DispatchKeyEvent(input.KeyUp).
			WithKey("Escape").WithCode("Escape").WithWindowsVirtualKeyCode(27),
	); err != nil {
		s.log("ClearSelection: Escape key error: %v", err)
		return err
	}
	// Scroll back to top so the next batch starts from a known position
	// before we start scanning the DOM again.
	return chromedp.Run(s.browserCtx, chromedp.Evaluate(`window.scrollTo(0, 0)`, nil))
}

func (s *Scraper) click(x, y float64) error {
	return chromedp.Run(s.browserCtx,
		input.DispatchMouseEvent(input.MousePressed, x, y).
			WithButton(input.Left).WithClickCount(1),
		input.DispatchMouseEvent(input.MouseReleased, x, y).
			WithButton(input.Left).WithClickCount(1),
	)
}


// TriggerBulkDownload triggers the bulk download for the current selection.
// It tries, in order: (1) any directly-visible Download button or aria-label
// in the selection toolbar, then (2) the kebab/more-options menu followed by
// its Download item. Both paths use trusted CDP clicks. Shift+D is *not*
// used: that shortcut only works inside the photo viewer and silently
// no-ops on a grid selection.
//
// Caller must have called SetupDownloads first so the resulting zip is
// captured to disk.
func (s *Scraper) TriggerBulkDownload() error {
	s.log("TriggerBulkDownload: starting")
	if err := s.requireSelection(); err != nil {
		s.log("TriggerBulkDownload: requireSelection failed: %v", err)
		return err
	}

	// Strategy 1: a directly-visible "Download" control in the toolbar.
	if clicked, err := s.clickElementByJS(downloadDirectJS); err != nil {
		s.log("TriggerBulkDownload: directDownload click error: %v", err)
		return err
	} else if clicked {
		s.log("TriggerBulkDownload: clicked direct Download button (no kebab needed)")
		return nil
	}
	s.log("TriggerBulkDownload: no direct Download button — falling back to kebab menu")

	// Strategy 2: kebab (more options) → Download menu item.
	opened, err := s.clickElementByJS(kebabJS)
	if err != nil {
		s.log("TriggerBulkDownload: kebab click error: %v", err)
		return err
	}
	if !opened {
		s.log("TriggerBulkDownload: kebab not found — diag: %s", s.diagnoseAfterSelect())
		return fmt.Errorf("could not locate the more-options (kebab) button — diag: %s",
			s.diagnoseAfterSelect())
	}
	s.log("TriggerBulkDownload: kebab clicked, waiting 700ms for menu")
	time.Sleep(700 * time.Millisecond)

	clicked, err := s.clickElementByJS(downloadMenuItemJS)
	if err != nil {
		s.log("TriggerBulkDownload: download menu item click error: %v", err)
		return err
	}
	if !clicked {
		s.log("TriggerBulkDownload: Download item missing in kebab menu — diag: %s", s.diagnoseAfterSelect())
		return fmt.Errorf("opened kebab menu but could not find a Download item — diag: %s",
			s.diagnoseAfterSelect())
	}
	s.log("TriggerBulkDownload: Download menu item clicked")
	return nil
}

// requireSelection fails fast if the page is not in selection mode. Without
// a selection, the kebab and Download controls don't even render.
func (s *Scraper) requireSelection() error {
	ok, err := s.selectionVisible()
	if err != nil {
		s.log("requireSelection: selectionVisible error: %v", err)
		return err
	}
	s.log("requireSelection: selectionVisible=%v", ok)
	if !ok {
		s.log("requireSelection: ABORT — toolbar not visible. diag: %s", s.diagnoseAfterSelect())
		return errors.New("selection toolbar is not visible — selection didn't take, " +
			"so there is nothing to download")
	}
	return nil
}

const downloadDirectJS = `
	const wanted = [
		'download', 'fazer o download', 'baixar',
		'descargar', 'télécharger', 'telecharger',
		'herunterladen', 'scarica',
	];
	const all = document.querySelectorAll('button, [role="button"], [aria-label]');
	for (const el of all) {
		const al = (el.getAttribute('aria-label') || '').toLowerCase();
		const tx = (el.innerText || '').trim().toLowerCase();
		for (const w of wanted) {
			if (al === w || tx === w || al.startsWith(w + ' ') || tx.startsWith(w + ' ')) {
				const r = el.getBoundingClientRect();
				if (r.width > 0 && r.height > 0) return el;
			}
		}
	}
	return null;
`

const kebabJS = `
	const isVisible = (el) => {
		const r = el.getBoundingClientRect();
		return r.width > 0 && r.height > 0;
	};
	const labels = [
		'More options', 'Mais opções', 'Más opciones',
		"Plus d'options", 'Weitere Optionen', 'Più opzioni',
	];
	// Google Photos pre-renders multiple hidden "More options" buttons (one
	// per photo's hover menu, etc.). querySelector returns the first match
	// regardless of visibility, so use querySelectorAll and take the first
	// *visible* one.
	for (const l of labels) {
		for (const el of document.querySelectorAll('[aria-label="' + l + '"]')) {
			if (isVisible(el)) return el;
		}
	}
	for (const el of document.querySelectorAll('[aria-label]')) {
		const al = (el.getAttribute('aria-label') || '').toLowerCase();
		if ((al.includes('more options') ||
			 al.includes('mais opções') || al.includes('mais opcoes') ||
			 al.includes('más opciones') || al.includes('mas opciones') ||
			 al.includes('weitere optionen') ||
			 al.includes("plus d'options") || al.includes('plus doptions'))
			&& isVisible(el)) {
			return el;
		}
	}
	return null;
`

const downloadMenuItemJS = `
	const isVisible = (el) => {
		const r = el.getBoundingClientRect();
		return r.width > 0 && r.height > 0;
	};
	const wanted = [
		'download', 'fazer o download', 'baixar',
		'descargar', 'télécharger', 'telecharger',
		'herunterladen', 'scarica',
	];
	for (const it of document.querySelectorAll('[role="menuitem"]')) {
		if (!isVisible(it)) continue;
		const t = ((it.innerText || it.textContent || '') + '').trim().toLowerCase();
		for (const w of wanted) {
			if (t === w || t.startsWith(w + ' ') || t.includes(w)) return it;
		}
	}
	return null;
`

// clickElementByJS evaluates `expr` (which must be the body of a () => { ... }
// expression returning a DOM element or null), scrolls that element into
// view, and clicks at its center via a trusted CDP mouse event. Returns
// (true, nil) if the element was found and clicked.
func (s *Scraper) clickElementByJS(expr string) (bool, error) {
	var r struct {
		X, Y, W, H float64
	}
	js := fmt.Sprintf(`(() => {
		const el = (() => { %s })();
		if (!el) return {X:0,Y:0,W:0,H:0};
		el.scrollIntoView({block:'center', inline:'center'});
		const b = el.getBoundingClientRect();
		return {X: b.left + b.width/2, Y: b.top + b.height/2, W: b.width, H: b.height};
	})()`, expr)
	if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &r)); err != nil {
		return false, err
	}
	if r.W == 0 {
		return false, nil
	}
	time.Sleep(150 * time.Millisecond)
	return true, s.click(r.X, r.Y)
}

// DownloadEvent is a state update for an in-flight download, derived from
// filesystem polling of the output directory. The CDP
// Browser.downloadWillBegin / downloadProgress events were unreliable in
// this chromedp version, so the watch is implemented as a directory poll
// in tui.go (see watchDownloads).
type DownloadEvent struct {
	GUID     string
	Filename string
	State    string // "inProgress", "completed"
	Received int64
	Total    int64
}

// ReadPreparingDownloadCount polls for Google Photos' "Preparing to
// download N items…" toast and returns N. This toast appears right after
// the bulk-download click and is the most reliable count we can get —
// it's how many items Google itself considers part of *this* download
// request, after deduping live-photo pairs / burst groups / shared items
// it can't export. Returns -1 if no parseable toast appears within timeout.
func (s *Scraper) ReadPreparingDownloadCount(timeout time.Duration) (int, error) {
	s.log("ReadPreparingDownloadCount: polling for 'preparing N items' toast (timeout=%s)", timeout)
	const js = `(() => {
		const isVisible = (el) => {
			const r = el.getBoundingClientRect();
			return r.width > 0 && r.height > 0;
		};
		// "Preparando o download de 5 itens" / "Preparing to download 5 items"
		// / "Preparando la descarga de 5 elementos" / etc. The number is the
		// thing we care about; the surrounding word is locale-dependent.
		const rx = /\b(\d+)\s+(?:iten[s]?|item[s]?|elementos?|éléments?|elementen?|elementi?|fotos?|photos?)\b/i;
		// Toasts/snackbars usually carry one of these roles.
		const sels = [
			'[role="alert"]', '[role="status"]',
			'[aria-live="polite"]', '[aria-live="assertive"]',
		];
		const seen = new Set();
		for (const sel of sels) {
			for (const el of document.querySelectorAll(sel)) {
				if (seen.has(el)) continue;
				seen.add(el);
				if (!isVisible(el)) continue;
				const t = ((el.innerText || el.textContent) || '').trim();
				if (!t) continue;
				const m = t.match(rx);
				if (m) return parseInt(m[1], 10);
			}
		}
		return -1;
	})()`
	deadline := time.Now().Add(timeout)
	for {
		if err := s.browserCtx.Err(); err != nil {
			return -1, err
		}
		var n int
		if err := chromedp.Run(s.browserCtx, chromedp.Evaluate(js, &n)); err == nil && n >= 0 {
			s.log("ReadPreparingDownloadCount: toast says %d items", n)
			return n, nil
		}
		if time.Now().After(deadline) {
			s.log("ReadPreparingDownloadCount: no parseable toast appeared within %s", timeout)
			return -1, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ApplyZoom injects a CSS rule that scales photos.google.com via the `zoom`
// property. With zoom < 1, more thumbnails fit per viewport — useful with
// -no-scroll, where each batch is bounded by visible items.
//
// CSS `zoom` is layout-affecting (unlike transform: scale), so
// getBoundingClientRect returns coordinates in the zoomed space — which is
// also the coordinate space CDP Input.dispatchMouseEvent uses, so existing
// click code keeps working without any offset adjustments.
//
// factor <= 0 or factor == 1 is a no-op.
func (s *Scraper) ApplyZoom(factor float64) error {
	if factor <= 0 || factor == 1.0 {
		return nil
	}
	js := fmt.Sprintf(`(() => {
		const id = 'gphotos-downloader-zoom';
		let el = document.getElementById(id);
		if (!el) {
			el = document.createElement('style');
			el.id = id;
			document.head.appendChild(el);
		}
		el.textContent = 'html, body { zoom: %g !important; }';
	})()`, factor)
	return chromedp.Run(s.browserCtx, chromedp.Evaluate(js, nil))
}

// SetupDownloads tells Chrome to send any download initiated by the page
// straight to outputDir without prompting. The actual progress tracking
// is done by polling the directory in tui.go — see watchDownloads.
func (s *Scraper) SetupDownloads(outputDir string) error {
	s.log("SetupDownloads: setting browser download path to %s", outputDir)
	return chromedp.Run(s.browserCtx,
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllow).
			WithDownloadPath(outputDir),
	)
}
