package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"github.com/adrg/xdg"
	"github.com/google/go-github/v80/github"
	"github.com/sewnie/wine"
	"github.com/vinegarhq/vinegar/internal/dirs"
	"github.com/vinegarhq/vinegar/internal/gutil"
	"github.com/vinegarhq/vinegar/internal/logging"
	"github.com/vinegarhq/vinegar/internal/netutil"

	. "github.com/pojntfx/go-gettext/pkg/i18n"
)

// Reports whether a Wine Prefix was initialized.
func (a *app) prepareWine() (bool, error) {
	firstRun := !a.pfx.Exists()

	a.boot.message(L("Setting up Wine"), "first-time", firstRun)

	cmd := a.pfx.Wine("")
	if string(a.cfg.Studio.WineRoot) == dirs.WinePath && cmd.Err != nil {
		if err := a.updateWine("Latest"); err != nil {
			return false, fmt.Errorf("dl: %w", err)
		}
	}
	if a.pfx.Running() {
		return false, nil
	}

	if err := a.pfx.Prepare(); err != nil {
		return firstRun, err
	}
	a.updateWineTheme()

	// Do _not_ do this on prefixes that already exist, only new ones,
	// as to not discard the existing appdata directory.
	if !firstRun {
		return false, nil
	}

	folders := wine.NewRegistryKey(
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`)
	folders.SetValue("Local AppData", dirs.Windows(dirs.AppDataPath))
	folders.SetValue("Documents", dirs.Windows(xdg.UserDirs.Documents))
	folders.SetValue("My Pictures", dirs.Windows(xdg.UserDirs.Pictures))

	a.boot.message(L("Updating Paths"))
	err := a.pfx.RegistryImportKey(folders)
	if err != nil {
		return true, fmt.Errorf("paths: %w", err)
	}

	// Restart wineserver for wineboot to update app data internally.
	if err := a.pfx.Boot(wine.BootRestart).Run(); err != nil {
		return true, fmt.Errorf("restart: %w", err)
	}

	if err := a.boot.restoreSettings(); err != nil {
		return true, fmt.Errorf("restore: %w", err)
	}

	return true, nil
}

func (a *app) updateWine(needle string) error {
	client := github.NewClient(nil)
	ctx := context.Background()

	var release *github.RepositoryRelease
	var err error
	if needle == "Latest" {
		release, _, err = client.Repositories.GetLatestRelease(ctx, "vinegarhq", "kombucha")
	} else {
		release, _, err = client.Repositories.GetReleaseByTag(ctx, "vinegarhq", "kombucha", needle)
	}
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}

	tag := release.GetTagName()
	dir := filepath.Join(dirs.Data, dirs.TagPrefix+tag)

	log := slog.With("tag", tag, "released", release.PublishedAt.Time)

	if _, err := os.Stat(dirs.WinePath); err == nil {
		path, err := filepath.EvalSymlinks(dirs.WinePath)
		if err != nil {
			return fmt.Errorf("readlink: %w", err)
		}
		name := filepath.Base(path)
		if name == filepath.Base(dir) {
			log.Info("Wine build up to date")
			return nil
		}

		slog.Info("Removing old Wine build", "name", name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove build: %w", err)
		}

		if err := os.Remove(dirs.WinePath); err != nil {
			return fmt.Errorf("remove link: %w", err)
		}
	}

	if _, err := os.Stat(dir); err == nil {
		goto install
	}

	if len(release.Assets) != 1 ||
		release.Assets[0].GetContentType() != "application/x-xz" {
		return errors.New("expected single .tar.xz release")
	}

	log.Info("Fetching Wine build")
	if err := netutil.ExtractURL(
		release.Assets[0].GetBrowserDownloadURL(), dirs.Data,
	); err != nil {
		return err
	}

install:
	if err := os.Symlink("kombucha-"+tag, dirs.WinePath); err != nil {
		return fmt.Errorf("create link: %w", err)
	}

	log.Info("Updated Wine build configuration")

	// No need to save wineroot is already set
	if a.cfg.Studio.WineRoot == dirs.WinePath {
		return nil
	}

	if path := dirs.WinePath; a.mgr != nil {
		// Thanks to the signals setup in the bindings procedure, this
		// will also handle setting the wine root as well.
		gutil.IdleAdd(func() {
			row := gutil.GetObject[adw.ActionRow](a.mgr.builder, "wine_row")
			row.SetSubtitle(path)
		})
		return nil
	}
	a.cfg.Studio.WineRoot = dirs.WinePath

	// Reload current configuration and save
	a.ActivateAction("win.save", nil)

	return nil
}

// Implements io.Writer for reading the log from Wine
func (a *app) Write(b []byte) (int, error) {
	for line := range strings.SplitSeq(string(b[:len(b)-1]), "\n") {
		// On older versions of Roblox, the channel used to be 'debugstr'.
		// XXXX:warn:seh:OutputDebugStringA "2026-04-27T14:57:01.636Z ... [FLog::Foo] Message"
		if a.boot != nil && len(line) >= 14 && line[10:13] == "seh" {
			// Ignore short messages such as newlines
			if len(line) < 39 {
				continue
			}
			from := line[14 : 14+18]
			if from != "OutputDebugStringA" {
				continue
			}
			a.boot.handleRobloxLog(line[34 : len(line)-1])
			continue
		}

		a.handleWineLog(line)
	}
	return len(b), nil
}

func (a *app) handleWineLog(line string) {
	if strings.Contains(line, "to unimplemented function advapi32.dll.SystemFunction036") {
		err := errors.New("Your Wineprefix is corrupt! Please delete all data in Vinegar's settings.")
		gutil.IdleAdd(func() {
			a.pfx.Server(wine.ServerKill, "9")
			a.showError(err)
		})
	}

	slog.Log(context.Background(), logging.LevelWine.Level(), line)
}

func (a *app) updateWineTheme() {
	// If the studio theme is "Default", the wine theme change will effect
	// studio as well.

	if !a.pfx.Running() {
		slog.Debug("Not changing theme: Wine is not running")
	}
	root := wine.RegistryKey{Name: "HKEY_CURRENT_USER"}
	v := root.Add(`Software\Microsoft\Windows\CurrentVersion`)
	pz := v.Add(`Themes\Personalize`)
	mgr := v.Add(`ThemeManager`)

	mgr.SetValue("ColorName", nil)
	mgr.SetValue("DllName", nil)
	mgr.SetValue("LoadedBefore", nil)
	mgr.SetValue("SizeName", nil)

	// Reloading an MSStyle requires calling windows API, hence why
	// I try to stick to only changing the current theme to none
	// and change the colors. This means that the light theme's clean
	// look will be gone in the light theme.
	mgr.SetValue("LoadedBefore", "0")
	mgr.SetValue("ThemeActive", "0")
	if a.GetStyleManager().GetDark() {
		root.Add(`Control Panel\Colors`).Values = darkThemeValues
		pz.SetValue("AppsUseLightTheme", uint32(0))
		pz.SetValue("SystemUsesLightTheme", uint32(0))
	} else {
		root.Add(`Control Panel\Colors`).Values = lightThemeValues
		pz.SetValue("AppsUseLightTheme", uint32(1))
		pz.SetValue("SystemUsesLightTheme", uint32(1))
	}

	slog.Debug("Changing Wine's theme", "dark", a.GetStyleManager().GetDark())
	if err := a.pfx.RegistryImportKey(&root); err != nil {
		slog.Error("Failed to change Wine's theme", "err", err)
	}

	// Modifying the registry manually triggers no events. To make Studio
	// refresh its theme, run a small application to open a GUI for just a
	// second. Yes this results in a flicker and requires the user to
	// focus on Studio, but there's no other way to allow live updates :/
	if a.boot.count > 0 {
		_ = a.pfx.Wine("start", "cmd", "/c", "exit").Run()
	}
}

var darkThemeValues = []wine.RegistryValue{
	{Name: "ActiveBorder", Data: "34 34 38"},            // #222226 window_bg_color
	{Name: "ActiveTitle", Data: "46 46 50"},             // #2e2e32 headerbar_bg_color
	{Name: "AppWorkSpace", Data: "45 45 49"},            // midpoint between window/view background
	{Name: "Background", Data: "29 29 32"},              // #1d1d20 view_bg_color
	{Name: "ButtonAlternativeFace", Data: "98 160 234"}, // accent blue from Adwaita
	{Name: "ButtonDkShadow", Data: "22 22 25"},          // slightly darker than view_bg
	{Name: "ButtonFace", Data: "46 46 50"},              // #2e2e32 headerbar
	{Name: "ButtonHilight", Data: "57 57 61"},           // lifted tone from card_bg_color overlay
	{Name: "ButtonLight", Data: "53 53 57"},
	{Name: "ButtonShadow", Data: "25 25 28"},
	{Name: "ButtonText", Data: "255 255 255"},
	{Name: "GradientActiveTitle", Data: "46 46 50"},
	{Name: "GradientInactiveTitle", Data: "40 40 44"}, // sidebar_backdrop_color #28282c
	{Name: "GrayText", Data: "136 136 136"},
	{Name: "Hilight", Data: "98 160 234"}, // accent blue
	{Name: "HilightText", Data: "255 255 255"},
	{Name: "InactiveBorder", Data: "40 40 44"}, // sidebar_backdrop
	{Name: "InactiveTitle", Data: "40 40 44"},
	{Name: "InactiveTitleText", Data: "211 211 211"},
	{Name: "InfoText", Data: "255 255 255"},
	{Name: "InfoWindow", Data: "54 54 58"}, // dialog_bg_color #36363a
	{Name: "Menu", Data: "54 54 58"},       // dialog/popover background
	{Name: "MenuBar", Data: "46 46 50"},    // keep menu slightly lighter
	{Name: "MenuHilight", Data: "98 160 234"},
	{Name: "MenuText", Data: "255 255 255"},
	{Name: "Scrollbar", Data: "46 46 50"},
	{Name: "TitleText", Data: "255 255 255"},
	{Name: "Window", Data: "29 29 32"},      // view_bg_color
	{Name: "WindowFrame", Data: "46 46 50"}, // subtle outline color
	{Name: "WindowText", Data: "255 255 255"},
}

var lightThemeValues = []wine.RegistryValue{
	{Name: "ActiveBorder", Data: "255 255 255"},
	{Name: "ActiveTitle", Data: "50 150 250"},
	{Name: "AppWorkSpace", Data: "128 128 128"},
	{Name: "Background", Data: "37 111 149"},
	{Name: "ButtonAlternateFace", Data: "255 255 255"},
	{Name: "ButtonAlternativeFace", Data: "98 160 234"},
	{Name: "ButtonDkShadow", Data: "106 106 106"},
	{Name: "ButtonFace", Data: "245 245 245"},
	{Name: "ButtonHilight", Data: "255 255 255"},
	{Name: "ButtonLight", Data: "227 227 227"},
	{Name: "ButtonShadow", Data: "166 166 166"},
	{Name: "ButtonText", Data: "0 0 0"},
	{Name: "GradientActiveTitle", Data: "50 150 250"},
	{Name: "GradientInactiveTitle", Data: "128 128 128"},
	{Name: "GrayText", Data: "106 106 106"},
	{Name: "Hilight", Data: "48 150 250"},
	{Name: "HilightText", Data: "255 255 255"},
	{Name: "HotTrackingColor", Data: "48 150 250"},
	{Name: "InactiveBorder", Data: "255 255 255"},
	{Name: "InactiveTitle", Data: "128 128 128"},
	{Name: "InactiveTitleText", Data: "200 200 200"},
	{Name: "InfoText", Data: "0 0 0"},
	{Name: "InfoWindow", Data: "255 255 255"},
	{Name: "Menu", Data: "255 255 255"},
	{Name: "MenuBar", Data: "255 255 255"},
	{Name: "MenuHilight", Data: "48 150 250"},
	{Name: "MenuText", Data: "0 0 0"},
	{Name: "Scrollbar", Data: "255 255 255"},
	{Name: "TitleText", Data: "0 0 0"},
	{Name: "Window", Data: "255 255 255"},
	{Name: "WindowFrame", Data: "158 158 158"},
	{Name: "WindowText", Data: "0 0 0"},
}
