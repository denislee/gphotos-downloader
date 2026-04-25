# gphotos-downloader

A small Bubble Tea TUI that drives a headless Chrome (via `chromedp`) to scan
a personal Google Photos library and download every original-resolution photo
and video to disk.

## How it works

1. The TUI launches Chrome with a persistent profile dir
   (`~/.gphotos-downloader/chrome-profile` by default). The first time you run
   it, pick option **[1] Login** — Chrome opens visibly so you can sign in
   (including 2FA). Once `photos.google.com` shows the photos grid, the TUI
   moves on automatically.
2. The scraper scrolls the main grid and collects every `/photo/<id>` link
   until no new ones appear for a few iterations.
3. For each photo, it opens the photo page, finds the largest
   `googleusercontent.com` image (or a `<video>` element), and rewrites the
   image URL with the `=d` suffix to request the original resolution.
4. Cookies + user-agent are copied out of the Chrome session into a
   `net/http` client, which streams each asset to `~/Pictures/gphotos-download`.
   Existing non-empty files are skipped.

After a successful login, choose **[2] Run headless** to reuse the saved
profile without opening a window.

## Build & run

```sh
go build ./...
./gphotos-downloader
```

Override paths with env vars:

```sh
GPHOTOS_OUTPUT_DIR=/data/photos GPHOTOS_PROFILE_DIR=/data/chrome ./gphotos-downloader
```

## Caveats

- The Google Photos web UI is unstable; selectors may break and need updating.
- Heavy scraping of your own account may trip rate limiting. Downloads are
  serial on purpose.
- This tool is for personal-account use. Google's official Photos APIs are the
  supported route for anything beyond a personal export.
