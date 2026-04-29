package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type stage int

const (
	stageMenu stage = iota
	stageLogin
	stageSelecting
	stageDownloading
	stageDone
	stageError
)

type model struct {
	ctx     context.Context
	cfg     Config
	program *tea.Program
	stage   stage
	spinner spinner.Model
	prog    progress.Model

	scraper  *Scraper
	headless bool
	logLines []string
	// debugLog is an opened file under cfg.OutputDir/debug.log; every
	// appended log line is also flushed here so a crashed run leaves a
	// complete trace on disk for the user to share.
	debugLog *os.File

	scrolledCount int
	selectedCount int
	downloads     map[string]*dlState

	err error
}

type dlState struct {
	Filename       string
	State          string
	Received, Total int64
}

type (
	loginDoneMsg      struct{}
	selectProgressMsg struct{ scrolled int }
	selectDoneMsg     struct{ count int }
	dlEventMsg        DownloadEvent
	dlAllDoneMsg      struct{}
	logMsg            struct{ line string }
	errMsg            struct{ err error }
)

func initialModel(ctx context.Context, cfg Config) model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))

	pr := progress.New(progress.WithDefaultGradient())
	pr.Width = 50

	return model{
		ctx:       ctx,
		cfg:       cfg,
		stage:     stageMenu,
		spinner:   sp,
		prog:      pr,
		downloads: map[string]*dlState{},
	}
}

func (m *model) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			if m.scraper != nil {
				m.scraper.Close()
			}
			if m.debugLog != nil {
				_ = m.debugLog.Close()
			}
			return m, tea.Quit
		case "1":
			if m.stage == stageMenu {
				m.headless = false
				m.stage = stageLogin
				m.appendLog("launching Chrome (visible) for login...")
				go m.runFlow()
			}
		case "2":
			if m.stage == stageMenu {
				m.headless = true
				m.stage = stageLogin
				m.appendLog("launching headless Chrome (uses saved profile)...")
				go m.runFlow()
			}
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case loginDoneMsg:
		m.stage = stageSelecting
		m.appendLog("logged in. selecting photos...")
		return m, nil

	case selectProgressMsg:
		m.scrolledCount = msg.scrolled
		return m, nil

	case selectDoneMsg:
		m.selectedCount = msg.count
		m.stage = stageDownloading
		m.appendLog(fmt.Sprintf("selected ~%d items. triggering Shift+D...", msg.count))
		return m, nil

	case dlEventMsg:
		st, ok := m.downloads[msg.GUID]
		if !ok {
			st = &dlState{Filename: msg.Filename}
			m.downloads[msg.GUID] = st
			m.appendLog(fmt.Sprintf("download began: %s", msg.Filename))
		}
		if msg.Filename != "" {
			st.Filename = msg.Filename
		}
		st.State = msg.State
		st.Received = msg.Received
		st.Total = msg.Total
		if msg.State == "completed" {
			m.appendLog(fmt.Sprintf("download done: %s (%s)", st.Filename, humanBytes(st.Received)))
		} else if msg.State == "canceled" {
			m.appendLog(fmt.Sprintf("download canceled: %s", st.Filename))
		}
		return m, nil

	case dlAllDoneMsg:
		m.stage = stageDone
		m.appendLog("all downloads finished.")
		return m, nil

	case logMsg:
		m.appendLog(msg.line)
		return m, nil

	case errMsg:
		m.err = msg.err
		m.stage = stageError
		m.appendLog("error: " + msg.err.Error())
		return m, nil
	}
	return m, nil
}

func (m *model) View() string {
	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("212")).
		Render("Google Photos Downloader (TUI)")

	cfgLine := lipgloss.NewStyle().
		Faint(true).
		Render(fmt.Sprintf("profile: %s\noutput:  %s", m.cfg.UserDataDir, m.cfg.OutputDir))

	var body string
	switch m.stage {
	case stageMenu:
		body = "" +
			"  [1] Login (opens visible Chrome — use this the first time)\n" +
			"  [2] Run headless (only after a successful login)\n" +
			"  [q] Quit\n"
	case stageLogin:
		body = m.spinner.View() + " waiting for login... a Chrome window will open.\n" +
			"   Sign in, wait for the photos grid, then this advances automatically."
	case stageSelecting:
		body = fmt.Sprintf("%s scrolling and selecting...\n  visible so far: %d photos",
			m.spinner.View(), m.scrolledCount)
	case stageDownloading:
		body = m.renderDownloads()
	case stageDone:
		body = m.renderDownloads() + "\n\nfinished. press [q] to quit."
	case stageError:
		body = "error: " + m.err.Error() + "\n\npress [q] to quit."
	}

	logBox := lipgloss.NewStyle().
		Faint(true).
		Render(strings.Join(tail(m.logLines, 8), "\n"))

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		cfgLine,
		"",
		body,
		"",
		logBox,
	)
}

func (m *model) renderDownloads() string {
	if len(m.downloads) == 0 {
		return fmt.Sprintf("%s waiting for Google to prepare the download (this can take a while for large libraries)...",
			m.spinner.View())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "selection: ~%d items\n\n", m.selectedCount)
	for _, st := range m.downloads {
		var pct float64
		if st.Total > 0 {
			pct = float64(st.Received) / float64(st.Total)
		}
		fmt.Fprintf(&b, "  %s  %s\n  %s  %s / %s\n\n",
			stateGlyph(st.State),
			st.Filename,
			m.prog.ViewAs(pct),
			humanBytes(st.Received),
			humanBytes(st.Total),
		)
	}
	return b.String()
}

func stateGlyph(s string) string {
	switch s {
	case "completed":
		return "[done]"
	case "canceled":
		return "[skip]"
	default:
		return "[ .. ]"
	}
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "?"
	}
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (m *model) appendLog(line string) {
	stamp := time.Now().Format("15:04:05")
	formatted := fmt.Sprintf("[%s] %s", stamp, line)
	m.logLines = append(m.logLines, formatted)
	// Also persist to a debug log file. Open lazily on first call so we
	// don't error out at model construction if the OutputDir doesn't yet
	// exist (main.go normally creates it, but a custom OutputDir flag
	// path could lag).
	if m.debugLog == nil && m.cfg.OutputDir != "" {
		path := filepath.Join(m.cfg.OutputDir, "debug.log")
		// O_APPEND so re-runs accumulate in one trace; the user can rm
		// it between sessions if they want a clean log.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			m.debugLog = f
			fmt.Fprintf(f, "\n[%s] === gphotos-downloader debug log opened ===\n",
				time.Now().Format("2006-01-02 15:04:05"))
		}
	}
	if m.debugLog != nil {
		fmt.Fprintln(m.debugLog, formatted)
	}
}

func tail(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// --- background goroutine ---------------------------------------------------

func (m *model) send(msg tea.Msg) {
	if m.program != nil {
		m.program.Send(msg)
	}
}

// runFlow drives the entire login → select → bulk-download pipeline. It runs
// in its own goroutine and pushes typed messages back to the TUI via
// m.program.Send.
func (m *model) runFlow() {
	m.scraper = NewScraper(m.cfg, m.headless)
	// Forward chrome.go's internal debug stream into the TUI log + debug.log
	// file so the user can see (and share back) exactly which strategy fired
	// on each click, where a click ladder bailed, what the toolbar looked
	// like when a matcher missed, etc.
	m.scraper.Logger = func(line string) {
		m.send(logMsg{line: "dbg: " + line})
	}
	m.send(logMsg{line: fmt.Sprintf("debug log: %s", filepath.Join(m.cfg.OutputDir, "debug.log"))})
	if err := m.scraper.EnsureLoggedIn(5 * time.Minute); err != nil {
		m.scraper.Close()
		m.scraper = nil
		m.send(errMsg{err: fmt.Errorf("login: %w", err)})
		return
	}
	m.send(loginDoneMsg{})

	if err := m.scraper.ApplyZoom(m.cfg.Zoom); err != nil {
		m.send(logMsg{line: fmt.Sprintf("warn: apply zoom: %v", err)})
	}

	if err := m.scraper.SetupDownloads(m.cfg.OutputDir); err != nil {
		m.send(errMsg{err: fmt.Errorf("setup downloads: %w", err)})
		return
	}

	batchSize := m.cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 200
	}

	state, err := LoadState(m.cfg.StateFile)
	if err != nil {
		m.send(errMsg{err: fmt.Errorf("load state: %w", err)})
		return
	}
	if n := len(state.Done()); n > 0 {
		m.send(logMsg{line: fmt.Sprintf("state: %d photos already downloaded in past runs — will skip", n)})
	}

	batchNum := 0
	totalSelected := 0

	for {
		batchNum++
		m.send(logMsg{line: fmt.Sprintf("batch %d: selecting up to %d photos...", batchNum, batchSize)})

		// In TrashAfter mode we don't filter by the done set: anything we
		// select will be trashed regardless, so passing the filter would
		// just hide the photos that are actually blocking the grid.
		var doneFilter map[string]bool
		if !m.cfg.TrashAfter {
			doneFilter = state.Done()
		}

		res, err := m.scraper.SelectBatch(doneFilter, batchSize, func(c int) {
			m.send(selectProgressMsg{scrolled: totalSelected + c})
		})
		if err != nil {
			m.send(errMsg{err: fmt.Errorf("batch %d select: %w", batchNum, err)})
			return
		}
		ids := res.IDs
		if len(ids) == 0 {
			if batchNum == 1 {
				switch {
				case res.TotalSeen == 0:
					m.send(errMsg{err: errors.New("no photos appeared in the grid at all — login may not have actually completed (try option [1] again)")})
				case res.SkippedDone == res.TotalSeen && res.HitGridBottom:
					m.send(errMsg{err: fmt.Errorf("scanned %d photos, all already recorded in the state file — looks like the whole library is already downloaded. To re-download, edit or delete %s", res.TotalSeen, m.cfg.StateFile)})
				case res.SkippedDone == res.TotalSeen:
					m.send(errMsg{err: fmt.Errorf("scanned %d photos but every one was already in the state file, and Google stopped scrolling further. If your library is bigger than %d photos, try `rm %s` and re-run with -trash so previously-downloaded photos are cleaned out of the grid", res.TotalSeen, res.TotalSeen, m.cfg.StateFile)})
				default:
					m.send(errMsg{err: fmt.Errorf("saw %d photos, %d were filtered as already-done, but couldn't select the remaining %d — selection clicks may be failing", res.TotalSeen, res.SkippedDone, res.TotalSeen-res.SkippedDone)})
				}
				return
			}
			// Double-check exhaustion: a long trash sequence sometimes
			// leaves Google Photos with a stale virtualized grid that
			// reports zero new photos even when the library still has
			// more. Force a hard reload and re-attempt one batch before
			// declaring the run done.
			m.send(logMsg{line: fmt.Sprintf("batch %d: SelectBatch returned 0 — refreshing grid to confirm library is really exhausted...", batchNum)})
			if err := m.scraper.RefreshGrid(2 * time.Minute); err != nil {
				m.send(logMsg{line: fmt.Sprintf("warn: refresh during exhaustion check failed: %v — trusting the empty result", err)})
				break
			}
			// Give Google a beat to settle after the reload; a
			// SelectBatch immediately after navigate sometimes still
			// reads the in-flight loading state.
			time.Sleep(2 * time.Second)
			res2, err := m.scraper.SelectBatch(doneFilter, batchSize, func(c int) {
				m.send(selectProgressMsg{scrolled: totalSelected + c})
			})
			if err != nil {
				m.send(logMsg{line: fmt.Sprintf("warn: re-select after refresh failed: %v — treating library as exhausted", err)})
				break
			}
			if len(res2.IDs) == 0 {
				m.send(logMsg{line: fmt.Sprintf("confirmed: library exhausted (post-refresh saw %d photos, %d already done)", res2.TotalSeen, res2.SkippedDone)})
				break
			}
			m.send(logMsg{line: fmt.Sprintf("recovered: post-refresh found %d more photos — continuing", len(res2.IDs))})
			res = res2
			ids = res.IDs
		}
		totalSelected += len(ids)
		m.send(selectDoneMsg{count: totalSelected})
		switch {
		case res.ToolbarCount < 0:
			diag := m.scraper.diagnoseSelectionCount()
			m.send(logMsg{line: fmt.Sprintf("note: clicked %d photos; toolbar count unparseable — diag: %s", len(ids), diag)})
		case res.ToolbarCount != len(ids):
			m.send(logMsg{line: fmt.Sprintf("note: clicked %d photos but toolbar shows %d selected — Google may have grouped items or some clicks didn't take", len(ids), res.ToolbarCount)})
		}

		// Decide whether we need to actually trigger a download for this
		// batch. In TrashAfter mode, if every selected photo was already
		// downloaded in a previous run, skip the download (we have the
		// zip locally) and go straight to trashing.
		needDownload := true
		if m.cfg.TrashAfter {
			needDownload = false
			for _, id := range ids {
				if !state.Done()[id] {
					needDownload = true
					break
				}
			}
			if !needDownload {
				m.send(logMsg{line: fmt.Sprintf("batch %d: all %d photos already downloaded in past runs — skipping download, only trashing", batchNum, len(ids))})
			}
		}

		// Default: trash everything we selected. In lossy mode, after we
		// know how many media files actually landed on disk, this is
		// narrowed down to just the first N IDs so we never trash more
		// items from Google than we have local copies of.
		trashIDs := ids

		if needDownload {
			// Re-issue setDownloadBehavior right before each bulk
			// download. Some Chrome profile states (e.g. a stale
			// download-prompt preference) can cause the path we set at
			// startup to be ignored later, with the zip silently landing
			// in ~/Downloads instead of our OutputDir.
			if err := m.scraper.SetupDownloads(m.cfg.OutputDir); err != nil {
				m.send(logMsg{line: fmt.Sprintf("warn: re-set download path: %v", err)})
			}
			m.send(logMsg{line: fmt.Sprintf("batch %d: opening kebab → Download for %d photos...", batchNum, len(ids))})
			if err := m.scraper.TriggerBulkDownload(); err != nil {
				m.send(errMsg{err: fmt.Errorf("batch %d trigger: %w", batchNum, err)})
				return
			}

			// Read Google's "Preparing to download N items..." toast as
			// the authoritative count for this batch — it accounts for
			// click drops, live-photo grouping, shared items it can't
			// export, etc. The toolbar count and our click count are
			// both upstream guesses; the toast is the actual contract
			// for what's about to land in the zip.
			toastCount, _ := m.scraper.ReadPreparingDownloadCount(8 * time.Second)
			if toastCount > 0 {
				m.send(logMsg{line: fmt.Sprintf("batch %d: Google toast says preparing %d items for download", batchNum, toastCount)})
			}

			zips, err := m.watchDownloads(m.cfg.OutputDir)
			if err != nil {
				m.send(errMsg{err: fmt.Errorf("batch %d download: %w", batchNum, err)})
				return
			}

			// Extract the zip(s) into a sibling folder and collect the
			// list of actual media files written to disk. Those files —
			// not a count or our intent — are the source of truth for
			// "what did we successfully download". Everything downstream
			// (the trash gate, state recording, the per-batch manifest)
			// keys off this list.
			var allMedia []ExtractedMedia
			for _, z := range zips {
				zipPath := filepath.Join(m.cfg.OutputDir, z)
				media, total, err := extractZip(zipPath, m.cfg.OutputDir)
				if err != nil {
					m.send(errMsg{err: fmt.Errorf("batch %d extract %s: %w", batchNum, z, err)})
					return
				}
				m.send(logMsg{line: fmt.Sprintf("extracted %s: %d media files (of %d entries)", z, len(media), total)})
				allMedia = append(allMedia, media...)
			}
			totalMedia := len(allMedia)
			if totalMedia == 0 {
				if len(zips) == 0 {
					m.send(errMsg{err: fmt.Errorf("batch %d: no zip files appeared in %s — Chrome may have saved the download elsewhere (a stale 'ask where to save' preference in the chrome profile can override our download path). Check ~/Downloads or your Chrome download settings", batchNum, m.cfg.OutputDir)})
				} else {
					m.send(errMsg{err: fmt.Errorf("batch %d: extracted %d zip(s) but found no media files — refusing to record state or trash", batchNum, len(zips))})
				}
				return
			}

			// Per-batch manifest so the user can audit what we have on
			// disk vs. what we tried to select. Saves a file alongside
			// the extracted folders listing each on-disk filename and
			// the photo IDs we clicked for this batch. The order
			// indicates our best-effort filename↔ID alignment (zip
			// order vs. selection order; not guaranteed to be exact).
			if err := writeBatchManifest(m.cfg.OutputDir, batchNum, ids, allMedia); err != nil {
				m.send(logMsg{line: fmt.Sprintf("warn: write manifest: %v", err)})
			}
			// Gate trash on the most authoritative count we have:
			//   1. Google's "Preparing to download N items" toast — the
			//      best signal because it's Google's own statement of
			//      what this download contains (after live-photo
			//      grouping, shared-item exclusion, etc.).
			//   2. Toolbar "N selected" count — second-best.
			//   3. Click count — only if the above two failed.
			expected := toastCount
			if expected <= 0 {
				expected = res.ToolbarCount
			}
			if expected <= 0 {
				expected = len(ids)
			}
			if totalMedia < expected && m.cfg.TrashAfter && !m.cfg.LossyTrash {
				m.send(errMsg{err: fmt.Errorf("batch %d: extracted %d media files but %d are selected on the page — refusing to trash. Re-run with -lossy-trash (or GPHOTOS_LOSSY_TRASH=1) to forfeit the missing items and keep going; or with -batch 1 to process one photo at a time. Files so far in %s", batchNum, totalMedia, expected, m.cfg.OutputDir)})
				return
			}
			// Narrow trashIDs down to what we actually have on disk so
			// we never trash more from Google than we extracted locally.
			// Without this, lossy-trash would forfeit the un-extracted
			// items into Google's trash too — the user has asked for the
			// opposite: trash only what's verified on disk, leave the
			// rest in Google for the next pass to retry.
			if totalMedia < len(ids) {
				trashIDs = ids[:totalMedia]
			}

			if totalMedia < expected && m.cfg.TrashAfter {
				const grace = 5 * time.Second
				m.send(logMsg{line: fmt.Sprintf(
					"!! LOSSY TRASH GATE: extracted %d of %d expected media files — about to trash %d items in %s. Press Ctrl+C now to abort if this looks wrong (the missing %d remain safe in Google).",
					totalMedia, expected, len(trashIDs), grace, expected-totalMedia)})
				select {
				case <-m.ctx.Done():
					m.send(errMsg{err: m.ctx.Err()})
					return
				case <-time.After(grace):
				}
				m.send(logMsg{line: fmt.Sprintf("lossy: proceeding to trash %d photos; the other %d remain in Google for a future batch", len(trashIDs), expected-totalMedia)})
			} else if totalMedia < len(ids) {
				m.send(logMsg{line: fmt.Sprintf("warn: extracted %d media files (toolbar showed %d selected; we clicked %d)", totalMedia, res.ToolbarCount, len(ids))})
			}

			// Record only the IDs we have media files for. We can't map
			// zip filenames to photo IDs, so the recorded subset is
			// "first totalMedia of the IDs we clicked" — best-effort,
			// but at least the count matches what's on disk so the state
			// file doesn't claim photos we don't actually have.
			if err := state.Append(trashIDs); err != nil {
				m.send(logMsg{line: fmt.Sprintf("warn: persist state failed: %v", err)})
			}
		}

		if m.cfg.TrashAfter {
			// Two paths for the trash step:
			//
			//   1. We *skipped* the download — every ID was already in
			//      state. SelectBatch's selection is still intact and the
			//      toolbar still has its full action set; go straight to
			//      TrashSelection.
			//
			//   2. We *did* download. After the bulk-download click,
			//      Google leaves the page in an unreliable state: often
			//      the "Limpar seleção" widget stays visible while the
			//      action buttons (Trash, Share, …) are gone — a
			//      half-toolbar that SelectionVisible() can't distinguish
			//      from a real selection. Trusting that check here was
			//      causing TrashSelection to fire on a toolbar with no
			//      trash button and fail. So unconditionally clear and
			//      reselect to put the toolbar back into a known full
			//      state. This also handles the lossy-trash narrowing
			//      case (trashIDs may be a subset of ids).
			if needDownload {
				if err := m.scraper.ClearSelection(); err != nil {
					m.send(logMsg{line: fmt.Sprintf("warn: clear before reselect failed: %v", err)})
				}
				time.Sleep(700 * time.Millisecond)
				m.send(logMsg{line: fmt.Sprintf("batch %d: selecting %d photos for trash...", batchNum, len(trashIDs))})
				n, err := m.scraper.SelectByIDs(trashIDs, nil)
				if err != nil {
					m.send(errMsg{err: fmt.Errorf("batch %d reselect: %w", batchNum, err)})
					return
				}
				if n < len(trashIDs) {
					m.send(logMsg{line: fmt.Sprintf("warn: re-selected %d of %d photos — trashing what we have", n, len(trashIDs))})
				}
			}
			m.send(logMsg{line: fmt.Sprintf("batch %d: moving %d photos to trash...", batchNum, len(trashIDs))})
			// Retry the trash flow up to maxTrashAttempts. Each attempt:
			//   click trash button → confirm dialog → VerifyTrashed.
			// If any step fails (button miss, dialog dismissed without
			// commit, photos still in grid), drop selection state, reselect
			// only the IDs still visible in the grid, and retry. This
			// prevents the previous failure mode where a missed trash click
			// was silently followed by the next batch reselecting the same
			// photos.
			const maxTrashAttempts = 3
			trashSucceeded := false
			pendingTrash := append([]string(nil), trashIDs...)
			var lastTrashErr error
			for attempt := 1; attempt <= maxTrashAttempts; attempt++ {
				if attempt > 1 {
					if err := m.scraper.ClearSelection(); err != nil {
						m.send(logMsg{line: fmt.Sprintf("warn: clear before trash retry failed: %v", err)})
					}
					time.Sleep(700 * time.Millisecond)
					n, err := m.scraper.SelectByIDs(pendingTrash, nil)
					if err != nil {
						lastTrashErr = fmt.Errorf("reselect for trash retry: %w", err)
						m.send(logMsg{line: fmt.Sprintf("warn: trash retry %d/%d reselect failed: %v", attempt, maxTrashAttempts, err)})
						continue
					}
					if n == 0 {
						// SelectByIDs couldn't find the pending IDs in the
						// grid. That *usually* means a prior trash click
						// committed after all — but it can also mean the
						// grid hasn't loaded those rows yet. Confirm via
						// DOM check before treating this batch as done,
						// otherwise we move on to a new selection while
						// the previous trash is still uncommitted.
						remaining, err := m.scraper.VerifyTrashed(pendingTrash, 8*time.Second)
						if err != nil {
							lastTrashErr = fmt.Errorf("post-reselect verify: %w", err)
							m.send(logMsg{line: fmt.Sprintf("warn: trash retry %d/%d post-reselect verify failed: %v", attempt, maxTrashAttempts, err)})
							continue
						}
						if len(remaining) == 0 {
							m.send(logMsg{line: fmt.Sprintf("batch %d: confirmed trash committed (none of %d IDs visible in grid)", batchNum, len(pendingTrash))})
							trashSucceeded = true
							break
						}
						m.send(logMsg{line: fmt.Sprintf("warn: reselect found 0 but %d of %d IDs still in DOM — retrying trash", len(remaining), len(pendingTrash))})
						pendingTrash = remaining
						lastTrashErr = fmt.Errorf("reselect missed but %d IDs still in grid", len(remaining))
						continue
					}
					m.send(logMsg{line: fmt.Sprintf("batch %d: trash retry %d/%d on %d remaining photos", batchNum, attempt, maxTrashAttempts, n)})
				}
				if err := m.scraper.TrashSelection(); err != nil {
					lastTrashErr = err
					m.send(logMsg{line: fmt.Sprintf("warn: trash attempt %d/%d failed: %v", attempt, maxTrashAttempts, err)})
					continue
				}
				remaining, err := m.scraper.VerifyTrashed(pendingTrash, 8*time.Second)
				if err != nil {
					lastTrashErr = err
					m.send(logMsg{line: fmt.Sprintf("warn: post-trash check failed on attempt %d/%d: %v", attempt, maxTrashAttempts, err)})
					continue
				}
				if len(remaining) == 0 {
					trashSucceeded = true
					lastTrashErr = nil
					break
				}
				m.send(logMsg{line: fmt.Sprintf(
					"warn: %d of %d photos still in grid after trash attempt %d/%d — will retry",
					len(remaining), len(pendingTrash), attempt, maxTrashAttempts)})
				pendingTrash = remaining
				lastTrashErr = fmt.Errorf("%d photos still visible in grid after trash", len(remaining))
			}
			if !trashSucceeded {
				m.send(errMsg{err: fmt.Errorf("batch %d trash failed after %d attempts: %w", batchNum, maxTrashAttempts, lastTrashErr)})
				return
			}
			m.send(logMsg{line: fmt.Sprintf("batch %d: confirmed %d photos removed from grid", batchNum, len(trashIDs))})
			// The "Movendo para a lixeira" snackbar stays visible for the
			// duration of the server-side delete. Reloading or starting the
			// next selection while it's still up races the delete and the
			// new grid can come back showing stale thumbnails. Wait it out,
			// then hard-reload so the next batch starts from a clean grid.
			m.send(logMsg{line: fmt.Sprintf("batch %d: waiting for trash toast to clear before reloading", batchNum)})
			if err := m.scraper.WaitForTrashToastGone(60 * time.Second); err != nil {
				m.send(logMsg{line: fmt.Sprintf("warn: waiting for trash toast: %v", err)})
			}
			m.send(logMsg{line: fmt.Sprintf("batch %d: reloading photos.google.com before next selection", batchNum)})
			if err := m.scraper.RefreshGrid(2 * time.Minute); err != nil {
				m.send(logMsg{line: fmt.Sprintf("warn: post-trash reload failed: %v", err)})
			}
		} else {
			if err := m.scraper.ClearSelection(); err != nil {
				m.send(logMsg{line: fmt.Sprintf("warn: clear selection failed: %v", err)})
			}
		}
		// Small breather between batches so Google doesn't see them as one
		// burst and start rate-limiting us.
		time.Sleep(2 * time.Second)
	}
	m.send(dlAllDoneMsg{})
}

// watchDownloads watches outputDir for the zip file Chrome creates after
// the page triggers a download, and returns once every new file has
// stabilized and no `.crdownload` is still in flight. Returns the list of
// completed (non-`.crdownload`) filenames that arrived during this call —
// the caller uses that list to extract and verify before trashing. We poll
// the filesystem rather than rely on the CDP Browser.downloadProgress
// events, which were unreliable in this chromedp version.
func (m *model) watchDownloads(outputDir string) ([]string, error) {
	const (
		initialWait = 5 * time.Minute // Google can take a while to assemble large zips
		// Google can split a single bulk download into multiple zips
		// (`Photos-N-001.zip`, `Photos-N-002.zip`, ...) with dead air
		// between them while it prepares each part. 30s comfortably
		// covers the gap on a normal connection without making
		// single-zip batches feel sluggish.
		multipartIdle    = 30 * time.Second
		idleNotifyAfter  = 3 * time.Second  // tell the user we're waiting before they think it's hung
		preStartTickFreq = 20 * time.Second // periodic update while no download has appeared
		pollEvery        = 250 * time.Millisecond
	)
	// When the user opts out of waiting for split-archive parts, return as
	// soon as the single zip stabilizes. A short idle still matters so we
	// don't return before the file finishes flushing to disk.
	idleAfter := multipartIdle
	if m.cfg.NoMultipartWait {
		idleAfter = 1 * time.Second
	}

	// Snapshot what's already in the dir; anything we see beyond this is
	// from this batch's download. We compare by *modification time*
	// against the watch-start instant rather than by size: a same-named
	// re-download (e.g. Chrome reuses `Photos-3-001.zip` and the new zip
	// happens to be the same byte length as the previous run's leftover)
	// would otherwise be silently treated as "unchanged from before"
	// and dropped from the result list — leaving a real zip on disk
	// that we then claim never appeared.
	startedAt := time.Now()
	snapshot, err := snapshotDir(outputDir)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(initialWait)
	lastPreStartTick := time.Now()
	var (
		seenAny      bool
		lastChange   time.Time
		notifiedIdle bool
		sizes        = map[string]int64{}
	)
	m.send(logMsg{line: fmt.Sprintf("watching %s for incoming downloads (timeout %s)", outputDir, initialWait)})

	for {
		select {
		case <-m.ctx.Done():
			return nil, m.ctx.Err()
		case <-time.After(pollEvery):
		}

		entries, err := os.ReadDir(outputDir)
		if err != nil {
			return nil, fmt.Errorf("read output dir: %w", err)
		}

		anyInProgress := false
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			info, err := e.Info()
			if err != nil {
				continue
			}
			// Skip files that pre-date this watch. A pre-existing file
			// is "ours" only if it has been touched after we started
			// watching — same-name overwrites or re-downloads with an
			// identical byte length have the right mtime even when
			// snapshot[name] would have masked them out by size alone.
			if _, existed := snapshot[name]; existed && !info.ModTime().After(startedAt) {
				continue
			}
			isInProgress := strings.HasSuffix(name, ".crdownload")
			if isInProgress {
				anyInProgress = true
			}
			if prev, ok := sizes[name]; !ok || prev != info.Size() {
				sizes[name] = info.Size()
				lastChange = time.Now()
				seenAny = true
				state := "inProgress"
				if !isInProgress {
					state = "completed"
				}
				m.send(dlEventMsg{
					GUID:     name,
					Filename: name,
					State:    state,
					Received: info.Size(),
					Total:    info.Size(),
				})
			}
		}

		if !seenAny {
			if time.Since(lastPreStartTick) > preStartTickFreq {
				lastPreStartTick = time.Now()
				m.send(logMsg{line: fmt.Sprintf(
					"still waiting for first byte (%.0fs elapsed). Google may still be preparing the zip; if this never starts, the file is probably landing in ~/Downloads instead of %s",
					time.Since(startedAt).Seconds(), outputDir)})
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("no download appeared in %s within %s — "+
					"Google may not have started the zip yet, or Chrome saved it elsewhere "+
					"(try checking ~/Downloads, and disable 'ask where to save each file' in the Chrome profile at %s)",
					outputDir, initialWait, m.cfg.UserDataDir)
			}
			continue
		}
		// Once nothing is in flight, surface to the user that we're
		// holding open a window for additional zip parts. Without this,
		// the TUI sits silent for ~90s and looks hung after a small
		// download stabilizes.
		if seenAny && !anyInProgress && !notifiedIdle && time.Since(lastChange) > idleNotifyAfter && !m.cfg.NoMultipartWait {
			notifiedIdle = true
			m.send(logMsg{line: fmt.Sprintf(
				"download idle — waiting up to %ds for additional zip parts (Google sometimes splits bulk downloads)",
				int((idleAfter - idleNotifyAfter).Seconds()))})
		}
		if !anyInProgress && time.Since(lastChange) > idleAfter {
			// Double-check: hold for a short confirmation window and
			// re-scan. If any zip's size grew, a new file appeared, or a
			// `.crdownload` reappeared (Google sometimes flushes a second
			// part after a long pause), reset and keep watching. This
			// catches the "looks done but isn't" case where Chrome had
			// momentarily stopped writing.
			const confirmWait = 3 * time.Second
			m.send(logMsg{line: fmt.Sprintf(
				"download appears complete — re-checking for %s to confirm no late writes...",
				confirmWait)})
			if changed, err := m.recheckStable(outputDir, sizes, snapshot, startedAt, confirmWait); err != nil {
				return nil, err
			} else if changed {
				lastChange = time.Now()
				notifiedIdle = false
				continue
			}
			completed := make([]string, 0, len(sizes))
			for name := range sizes {
				if !strings.HasSuffix(name, ".crdownload") {
					completed = append(completed, name)
				}
			}
			m.send(logMsg{line: fmt.Sprintf("download confirmed stable: %d zip(s)", len(completed))})
			return completed, nil
		}
	}
}

// recheckStable holds for `wait` and verifies that the set of in-flight
// downloads hasn't changed: no new file in the watched directory, no zip
// growing, and no `.crdownload` reappearing. Returns true if anything moved
// during the window (caller should keep watching) or false if everything was
// stable (caller can return).
func (m *model) recheckStable(outputDir string, sizes map[string]int64, snapshot map[string]int64, startedAt time.Time, wait time.Duration) (bool, error) {
	const tick = 250 * time.Millisecond
	deadline := time.Now().Add(wait)
	for {
		select {
		case <-m.ctx.Done():
			return false, m.ctx.Err()
		case <-time.After(tick):
		}
		entries, err := os.ReadDir(outputDir)
		if err != nil {
			return false, fmt.Errorf("recheck read dir: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			info, err := e.Info()
			if err != nil {
				continue
			}
			// Same filter as watchDownloads: only files that belong to this watch.
			if _, existed := snapshot[name]; existed && !info.ModTime().After(startedAt) {
				continue
			}
			if strings.HasSuffix(name, ".crdownload") {
				m.send(logMsg{line: fmt.Sprintf("recheck: %s reappeared in flight — keeping watch", name)})
				return true, nil
			}
			prev, known := sizes[name]
			if !known {
				m.send(logMsg{line: fmt.Sprintf("recheck: new file %s appeared — keeping watch", name)})
				return true, nil
			}
			if info.Size() != prev {
				m.send(logMsg{line: fmt.Sprintf("recheck: %s size changed (%d → %d) — keeping watch", name, prev, info.Size())})
				return true, nil
			}
		}
		if time.Now().After(deadline) {
			return false, nil
		}
	}
}

func snapshotDir(dir string) (map[string]int64, error) {
	out := map[string]int64{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[e.Name()] = info.Size()
	}
	return out, nil
}
