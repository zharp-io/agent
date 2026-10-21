package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// `zharp-collector update` replaces the binary with the latest release and then has the new
// binary merge into the config whatever it can detect that isn't collected yet (cli_merge.go).
// If GitHub can't be reached or the download fails, nothing changes; if the new binary can't
// load the config, the old binary is put back.

const releaseRepo = "zharp-io/agent"

// Where releases come from; tests point these at a local server.
var (
	releaseAPIBase      = "https://api.github.com/repos/" + releaseRepo
	releaseDownloadBase = "https://github.com/" + releaseRepo + "/releases/download"
	releaseHTTP         = &http.Client{Timeout: 10 * time.Minute}
)

// cliUpdate handles `zharp-collector update [--check] [--version vX.Y.Z] [--no-app-logs]`.
// `update --apply [--from VERSION]` is the merge step on its own: update runs it in the new
// binary, and install.sh runs it after replacing the binary of an existing install.
func cliUpdate() {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	pin := fs.String("version", "", "install this release (vX.Y.Z) instead of the latest")
	check := fs.Bool("check", false, "show the installed and latest versions and what an update would add; change nothing")
	noAppLogsFlag := fs.Bool("no-app-logs", false, "don't add application logs (Docker containers, systemd services, PM2) (or ZHARP_NO_APP_LOGS=1)")
	apply := fs.Bool("apply", false, "merge step only, run by the new binary")
	from := fs.String("from", "", "version the binary was updated from (with --apply)")
	_ = fs.Parse(os.Args[2:])
	appLogs := !appLogsDisabled(*noAppLogsFlag)

	if !*check && runtime.GOOS != "windows" && os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "  [!] This command must be run as root. Try: sudo zharp-collector update")
		os.Exit(1)
	}
	if *apply {
		runApply(applyOptions{from: *from, appLogs: appLogs})
		return
	}
	if !fileExists(installConfigFile()) {
		fmt.Fprintf(os.Stderr, "  [!] No config found at %s. Run 'zharp-collector install' first.\n", installConfigFile())
		os.Exit(1)
	}
	cleanupOldBinary()

	fmt.Println()
	uiInfo("Checking for a new release...")
	latest, err := resolveRelease(*pin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Could not find the latest release on GitHub: %v\n", err)
		fmt.Fprintln(os.Stderr, "      Nothing was changed.")
		os.Exit(1)
	}
	download := needsDownload(version, latest, *pin != "")

	if *check {
		uiOK("Installed: " + version)
		uiOK("Latest:    " + latest)
		if download {
			uiDimMsg("What would be added is found with the installed version's detection; " + latest + " may find more.")
		}
		runApply(applyOptions{appLogs: appLogs, dryRun: true})
		if download {
			uiDimMsg("To update: " + cliPrefix() + "zharp-collector update")
			fmt.Println()
		}
		return
	}

	if !download {
		uiOK("Already on " + version + "; looking for anything new to collect.")
		runApply(applyOptions{appLogs: appLogs})
		return
	}

	target, err := installedBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Cannot determine the binary path: %v\n", err)
		os.Exit(1)
	}
	uiInfo(fmt.Sprintf("Downloading %s (%s/%s)...", latest, runtime.GOOS, runtime.GOARCH))
	tmp, err := downloadRelease(latest, runtime.GOOS, runtime.GOARCH, filepath.Dir(target))
	if err == nil {
		if err = checkBinary(tmp, latest); err != nil {
			_ = os.Remove(tmp)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [!] Download failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "      Nothing was changed.")
		os.Exit(1)
	}
	backup, err := swapBinary(target, tmp, runtime.GOOS)
	if err != nil {
		_ = os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "  [!] Could not replace %s: %v\n", target, err)
		fmt.Fprintln(os.Stderr, "      Nothing was changed.")
		os.Exit(1)
	}
	uiOK(fmt.Sprintf("Installed %s → %s", latest, target))

	// The merge runs in the new binary, so its detection is the newest.
	args := []string{"update", "--apply", "--from", version}
	if !appLogs {
		args = append(args, "--no-app-logs")
	}
	cmd := exec.Command(target, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if rbErr := rollbackBinary(target, backup, runtime.GOOS); rbErr != nil {
			uiWarn(fmt.Sprintf("Could not put %s back: %v (it is at %s)", version, rbErr, backup))
		} else {
			uiWarn("The update was not applied; " + version + " was put back.")
		}
		os.Exit(1)
	}
	if runtime.GOOS != "windows" {
		_ = os.Remove(backup)
	} // On Windows this process still runs from it; the next run removes it.
}

// resolveRelease is the pinned tag, or the latest release's tag from the GitHub API.
func resolveRelease(pin string) (string, error) {
	if pin = strings.TrimSpace(pin); pin != "" {
		if !strings.HasPrefix(pin, "v") {
			pin = "v" + pin
		}
		return pin, nil // a tag that doesn't exist fails at the download
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPIBase+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "zharp-collector/"+version)
	resp, err := releaseHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API: %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("GitHub API: %w", err)
	}
	if rel.TagName == "" {
		return "", errors.New("GitHub API: no tag_name in the latest release")
	}
	return rel.TagName, nil
}

// parseVersion reads vX.Y.Z (a -suffix is ignored).
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// compareVersions is -1, 0 or 1 as a is older, the same or newer than b; ok is false when
// either isn't a release version (a "dev" build).
func compareVersions(a, b string) (cmp int, ok bool) {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range va {
		switch {
		case va[i] < vb[i]:
			return -1, true
		case va[i] > vb[i]:
			return 1, true
		}
	}
	return 0, true
}

// needsDownload decides whether current has to be replaced by release: when it is older, or
// different from a pinned version. A build without a release version is always replaced.
func needsDownload(current, release string, pinned bool) bool {
	cmp, ok := compareVersions(current, release)
	if !ok {
		return true
	}
	if pinned {
		return cmp != 0
	}
	return cmp < 0
}

// releaseAsset is the release file for an OS and architecture, and for a .tar.gz the name of the
// binary inside it.
func releaseAsset(tag, goos, goarch string) (asset, inner string, err error) {
	switch goos + "/" + goarch {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64":
		return fmt.Sprintf("zharp-collector-%s-%s-%s.tar.gz", tag, goos, goarch), fmt.Sprintf("zharp-collector-%s-%s", goos, goarch), nil
	case "windows/amd64":
		return fmt.Sprintf("zharp-collector-%s-windows-amd64.exe", tag), "", nil
	}
	return "", "", fmt.Errorf("no release build for %s/%s", goos, goarch)
}

// downloadRelease fetches the binary into a temporary file in dir (the binary's own directory,
// so it can be renamed into place) and returns its path.
func downloadRelease(tag, goos, goarch, dir string) (string, error) {
	asset, inner, err := releaseAsset(tag, goos, goarch)
	if err != nil {
		return "", err
	}
	url := releaseDownloadBase + "/" + tag + "/" + asset
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "zharp-collector/"+version)
	resp, err := releaseHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}

	pattern := ".zharp-collector-update-*"
	if goos == "windows" {
		pattern += ".exe" // Windows only runs it (for the version check) with the extension
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	if inner == "" {
		_, err = io.Copy(f, resp.Body)
	} else {
		err = extractFromTarGz(resp.Body, inner, f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o755)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// extractFromTarGz copies the file called name out of a .tar.gz.
func extractFromTarGz(r io.Reader, name string, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s is not in the archive", name)
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == name {
			_, err = io.Copy(w, tr)
			return err
		}
	}
}

// checkBinary runs the download's --version: it must start on this machine and be tag.
func checkBinary(file, tag string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, file, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run: %v", err)
	}
	if !strings.Contains(string(out), strings.TrimPrefix(tag, "v")) {
		return fmt.Errorf("the downloaded binary reports %q, not %s", strings.TrimSpace(string(out)), tag)
	}
	return nil
}

// installedBinary is the binary being run, with symlinks resolved.
func installedBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

// swapBinary puts newFile at target and keeps the old binary at the returned path, so it can
// be put back. On Unix the old one is hard-linked aside and the new one renamed over it: the
// path never goes missing. Windows can't overwrite or delete a running .exe, but it can rename
// it, so the old one is renamed aside first; it is removed on the next run (cleanupOldBinary).
func swapBinary(target, newFile, goos string) (backup string, err error) {
	backup = target + ".old"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		// Still in use by a process from an earlier update (Windows): use a name of its own.
		backup = fmt.Sprintf("%s.old-%d", target, time.Now().UnixNano())
	}
	if goos == "windows" {
		if err := os.Rename(target, backup); err != nil {
			return "", err
		}
		if err := os.Rename(newFile, target); err != nil {
			_ = os.Rename(backup, target)
			return "", err
		}
		return backup, nil
	}
	if err := os.Link(target, backup); err != nil {
		if err := copyFile(target, backup); err != nil {
			return "", err
		}
	}
	if err := os.Rename(newFile, target); err != nil {
		_ = os.Remove(backup)
		return "", err
	}
	return backup, nil
}

// rollbackBinary puts the binary swapBinary set aside back at target.
func rollbackBinary(target, backup, goos string) error {
	if goos == "windows" {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(backup, target)
}

// cleanupOldBinary removes binaries an earlier update set aside. On Windows one may still be in
// use by a process that hasn't exited; it is left for the next run.
func cleanupOldBinary() {
	if exe, err := installedBinary(); err == nil {
		cleanupOldBinaryAt(exe)
	}
}

func cleanupOldBinaryAt(target string) {
	old, _ := filepath.Glob(target + ".old*")
	for _, f := range old {
		_ = os.Remove(f)
	}
}
