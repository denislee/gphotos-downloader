package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
)

func parseEnvBool(v string) (bool, bool) {
	switch v {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not resolve home dir:", err)
		os.Exit(1)
	}

	// Defaults, then env overrides, then flag overrides — flags win.
	cfg := Config{
		UserDataDir:     filepath.Join(home, ".gphotos-downloader", "chrome-profile"),
		OutputDir:       filepath.Join(home, "Pictures", "gphotos-download"),
		StateFile:       filepath.Join(home, ".gphotos-downloader", "downloaded.txt"),
		BatchSize:       200,
		TrashAfter:      true,
		LossyTrash:      true,
		NoScroll:        true,
		NoMultipartWait: true,
	}
	if v := os.Getenv("GPHOTOS_OUTPUT_DIR"); v != "" {
		cfg.OutputDir = v
	}
	if v := os.Getenv("GPHOTOS_PROFILE_DIR"); v != "" {
		cfg.UserDataDir = v
	}
	if v := os.Getenv("GPHOTOS_STATE_FILE"); v != "" {
		cfg.StateFile = v
	}
	if v := os.Getenv("GPHOTOS_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.BatchSize = n
		}
	}
	if b, ok := parseEnvBool(os.Getenv("GPHOTOS_DELETE_AFTER")); ok {
		cfg.TrashAfter = b
	}
	if b, ok := parseEnvBool(os.Getenv("GPHOTOS_LOSSY_TRASH")); ok {
		cfg.LossyTrash = b
	}
	if b, ok := parseEnvBool(os.Getenv("GPHOTOS_NO_SCROLL")); ok {
		cfg.NoScroll = b
	}
	if b, ok := parseEnvBool(os.Getenv("GPHOTOS_NO_MULTIPART_WAIT")); ok {
		cfg.NoMultipartWait = b
	}
	if v := os.Getenv("GPHOTOS_ZOOM"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			cfg.Zoom = f
		}
	}

	flag.StringVar(&cfg.OutputDir, "output", cfg.OutputDir,
		"directory where downloaded zip files are written")
	flag.StringVar(&cfg.OutputDir, "o", cfg.OutputDir,
		"shorthand for -output")
	flag.StringVar(&cfg.UserDataDir, "profile", cfg.UserDataDir,
		"Chrome user data directory (login session lives here)")
	flag.StringVar(&cfg.StateFile, "state", cfg.StateFile,
		"file tracking already-downloaded photo IDs")
	flag.IntVar(&cfg.BatchSize, "batch", cfg.BatchSize,
		"photos selected per zip download")
	flag.BoolVar(&cfg.TrashAfter, "trash", cfg.TrashAfter,
		"after each batch, move the downloaded photos to Google Photos' trash (recoverable for 60 days)")
	flag.BoolVar(&cfg.LossyTrash, "lossy-trash", cfg.LossyTrash,
		"if the zip ends up with fewer photos than were selected, trash anyway — forfeit the un-extracted items and keep going (data loss possible; recoverable from Google's trash for 60 days)")
	flag.BoolVar(&cfg.NoScroll, "no-scroll", cfg.NoScroll,
		"select only the photos currently visible in the viewport — don't scroll the virtualized grid for more. Pairs naturally with -trash: each batch trashes the viewport's worth, and the next batch starts on a fresh top of the grid")
	flag.BoolVar(&cfg.NoMultipartWait, "no-multipart-wait", cfg.NoMultipartWait,
		"return from a batch's download as soon as the first zip is stable, instead of holding a 30s idle window for follow-up `Photos-N-002.zip` parts. Faster when batches reliably produce one zip; misses the second part if Google does split it")
	flag.Float64Var(&cfg.Zoom, "zoom", cfg.Zoom,
		"CSS zoom factor applied to photos.google.com after login (e.g. 0.35 = 35% size, fitting more thumbnails per viewport). 0 or 1 disables")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), `gphotos-downloader: bulk-download a personal Google Photos library via Chrome (chromedp).

Usage:
  gphotos-downloader [flags]

Flags:`)
		flag.PrintDefaults()
		fmt.Fprintln(flag.CommandLine.Output(), `
Each flag has an env-var equivalent (GPHOTOS_OUTPUT_DIR, GPHOTOS_PROFILE_DIR,
GPHOTOS_STATE_FILE, GPHOTOS_BATCH_SIZE, GPHOTOS_DELETE_AFTER,
GPHOTOS_LOSSY_TRASH, GPHOTOS_NO_SCROLL, GPHOTOS_NO_MULTIPART_WAIT,
GPHOTOS_ZOOM). Flags take priority over env vars.

Examples:
  gphotos-downloader -o /data/photos
  gphotos-downloader -o /data/photos -trash -batch 100`)
	}
	flag.Parse()

	if err := os.MkdirAll(cfg.UserDataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir profile:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir output:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := initialModel(ctx, cfg)
	p := tea.NewProgram(&m, tea.WithAltScreen())
	m.program = p
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
		os.Exit(1)
	}
}
