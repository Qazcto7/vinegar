// Package config implements types and routines to configure Vinegar.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/sewnie/rbxbin"
	"github.com/sewnie/wine"
	"github.com/sewnie/wine/dxvk"
	"github.com/vinegarhq/vinegar/internal/dirs"
	"github.com/vinegarhq/vinegar/internal/logging"
	"github.com/vinegarhq/vinegar/internal/sysinfo"
)

const (
	// DXVK 3.0 rewrote shader compilation (dxbc-spirv) and now hard-
	// requires Vulkan 1.3. Confirmed on real hardware (an old NVIDIA
	// Kepler GT 740M, not just the Haswell iGPU this fork already
	// steers away from DXVK) that this makes DXVK 3.0.2 fail outright
	// with "No adapters found ... A Vulkan 1.3 capable setup is
	// required" - i.e. DXVK becomes entirely unusable on older GPUs,
	// not just slower. Staying on the last version without that
	// requirement.
	DXVKVersion      = "2.7.1"
	// v1.13.0 "Pacemaker" - confirmed working tarball name against the
	// actual release asset (dxvk-sarek-1.13.0.tar.gz). No "-async" suffix:
	// since v1.12.0 there is only a single unified build; the shader
	// compilation method (dyasync by default, tuned for weak CPUs/iGPUs)
	// is chosen at runtime via DXVK_SHADER_COMPILATION_METHOD instead of
	// shipping separate async/non-async tarballs.
	DXVKSarekVersion = "Sarek-1.13.0"
	WebViewVersion   = "144.0.3719.92"

	// A conservative size that fits inside virtually any real screen,
	// including small laptop panels (e.g. 1366x768). The original
	// 1814x1024 default exceeded common screen heights, which made the
	// Wine virtual desktop (a fixed-size window Wine itself is fully
	// responsible for sizing, not the Linux window manager) overflow the
	// display and appear to force Studio into fullscreen on every launch,
	// with no way to move or resize it. Users can still set a larger or
	// smaller size manually in Settings -> Advanced Wine Settings ->
	// Virtual Desktop.
	DesktopsResolution = "1280x720"
)

// Order must be the same as the renderer model in the configurator.
var RendererValues = []string{
	"D3D11",
	"D3D11FL10",
	"DXVK",
	"DXVK-Sarek",
	"Vulkan",
}

type Studio struct {
	WebView  string `toml:"webview"`
	WineRoot string `toml:"wineroot"`

	Renderer  string `toml:"renderer"`
	Desktop   string `toml:"virtual_desktop"`
	ForcedGpu string `toml:"gpu"`

	Launcher   string `toml:"launcher"`
	DiscordRPC bool   `toml:"discord_rpc"`
	GameMode   bool   `toml:"gamemode"`

	Env    map[string]string `toml:"env"`
	FFlags rbxbin.FFlags     `toml:"fflags"`

	ForcedVersion string `toml:"forced_version"`
	Channel       string `toml:"channel"`
}

type Config struct {
	Studio Studio `toml:"studio"`
	// Only adds to Studio.Env, reserved for backwards compatibility
	Env   map[string]string `toml:"env"`
	Debug bool              `toml:"debug"`
}

var (
	ErrWineRootAbs     = errors.New("wine root path is not an absolute path")
	ErrWineRootInvalid = errors.New("no wine binary present in wine root")
)

// Load will load the configuration file; if it doesn't exist, it
// will fallback to the default configuration.
func Load() (*Config, error) {
	cfg := Default()

	if _, err := os.Stat(dirs.ConfigPath); errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}

	if _, err := toml.DecodeFile(dirs.ConfigPath, &cfg); err != nil {
		return cfg, err
	}

	maps.Copy(cfg.Studio.Env, cfg.Env)
	cfg.Env = nil

	logging.LoggerLevel = slog.LevelInfo
	if cfg.Debug {
		logging.LoggerLevel = slog.LevelDebug
	}

	return cfg, nil
}

// Default returns a default configuration.
func Default() (cfg *Config) {
	cfg = &Config{
		Debug: false,

		Env: make(map[string]string),

		Studio: Studio{
			WebView:    WebViewVersion,
			GameMode:   true,
			Renderer:   "DXVK",
			Channel:    "",
			DiscordRPC: true,
			FFlags:     make(rbxbin.FFlags),
			Env:        make(map[string]string),
		},
	}
	// No need to select if there is only a single GPU, and to
	// prefer PRIME discrete behavior by default, on systems
	// where the first GPU is a iGPU.
	if len(sysinfo.Cards) >= 2 && sysinfo.Cards[0].Embedded {
		cfg.Studio.ForcedGpu = sysinfo.Cards[1].Addr()
	}

	// Prefer a stable, non-Vulkan renderer by default on GPUs that are
	// known to have incomplete Vulkan support, such as Intel Haswell
	// iGPUs (Mesa itself warns "Haswell Vulkan support is incomplete").
	// Forcing DXVK/Vulkan there leads to shader-compile stalls and
	// graphical lockups in Studio. Users who know their setup is fine
	// can still switch back to DXVK/Vulkan manually.
	//
	// Determining which card actually renders: if a GPU is explicitly
	// forced, use that one. Otherwise, on a hybrid/laptop system with no
	// forced GPU, rendering (and always display output) defaults to the
	// embedded GPU rather than whichever card happens to enumerate
	// first - naively checking Cards[0] missed the Haswell iGPU
	// entirely on systems where a discrete GPU (e.g. an older NVIDIA
	// Optimus card) enumerates before it.
	var renderCard sysinfo.Card
	var found bool
	switch {
	case cfg.Studio.ForcedGpu != "":
		for _, card := range sysinfo.Cards {
			if card.Addr() == cfg.Studio.ForcedGpu {
				renderCard, found = card, true
			}
		}
	default:
		for _, card := range sysinfo.Cards {
			if card.Embedded {
				renderCard, found = card, true
				break
			}
		}
		if !found && len(sysinfo.Cards) > 0 {
			renderCard, found = sysinfo.Cards[0], true
		}
	}
	if found && renderCard.IncompleteVulkan() {
		cfg.Studio.Renderer = "D3D11FL10"
	}

	// Wine's native Wayland driver (winewayland.drv), used automatically
	// whenever WAYLAND_DISPLAY is set, does not yet implement the
	// window-manager-mediated focus/input routing that WebView2's child
	// windows rely on: clicks and keyboard input silently fail to reach
	// the login page. Running Studio inside Wine's virtual desktop avoids
	// this entirely, since everything is then composited into a single
	// regular window. This is the same workaround documented on
	// Vinegar's Troubleshooting page; users on an X11/XWayland session
	// are unaffected and can disable this in Settings if desired.
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		cfg.Studio.Desktop = DesktopsResolution
	}

	// Default to use the VinegarHQ Kombucha builds to be
	// downloaded at runtime, on non musl systems.
	// Note: Author of this code uses a musl system. (me)
	if !strings.Contains(sysinfo.LibC, "musl") {
		cfg.Studio.WineRoot = dirs.WinePath
	}

	return
}

func (s *Studio) UnmarshalTOML(data any) error {
	// prevent recursion by typing
	type Alias Studio
	proxy := struct {
		*Alias
		DXVK string `toml:"dxvk"`
	}{Alias: (*Alias)(s)}

	// encode to and back to retrieve all options from raw TOML
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(data); err != nil {
		return err
	}
	if _, err := toml.Decode(buf.String(), &proxy); err != nil {
		return err
	}

	s = (*Studio)(proxy.Alias)

	if proxy.DXVK != "" {
		slog.Warn("The DXVK option alongside it's versioning has been deprecated, setting Renderer")
		s.Renderer = "DXVK"
	}
	if !slices.Contains(RendererValues, s.Renderer) {
		return fmt.Errorf("renderer must be one of %s", RendererValues)
	}
	return nil
}

func (s *Studio) LauncherPath() (string, error) {
	return exec.LookPath(strings.Fields(s.Launcher)[0])
}

func (s *Studio) DXVKVersion() string {
	switch s.Renderer {
	case "DXVK":
		return DXVKVersion
	case "DXVK-Sarek":
		return DXVKSarekVersion
	}
	return ""
}

func (c *Config) Prefix() *wine.Prefix {
	pfx := wine.New(
		path.Join(dirs.Prefixes, "studio"),
		string(c.Studio.WineRoot),
	)

	env := maps.Clone(c.Studio.Env)

	for _, card := range sysinfo.Cards {
		if string(c.Studio.ForcedGpu) != card.Addr() {
			continue
		}

		slog.Debug("Using GPU", "index", card.Index, "card", card.Product)
		env["DRI_PRIME"] = string(card.VendorID) + ":" + string(card.ProductID) + "!"

		// Unset problematic variables that may be set by switcheroo-control
		env["__VK_LAYER_NV_optimus"] = ""
		env["VK_LOADER_DRIVERS_SELECT"] = ""

		vendors := "/usr/lib/x86_64-linux-gnu/GL/glvnd/egl_vendor.d/"
		if !sysinfo.Flatpak {
			vendors = "/usr/share/glvnd/egl_vendor.d/"
		}

		if !strings.HasPrefix(card.Driver, "nvidia") {
			env["__EGL_VENDOR_LIBRARY_FILENAMES"] = vendors + "50_mesa.json"
			env["__GLX_VENDOR_LIBRARY_NAME"] = "mesa"
			env["__NV_PRIME_RENDER_OFFLOAD"] = "0"
		} else {
			env["__EGL_VENDOR_LIBRARY_FILENAMES"] = vendors + "10_nvidia.json"
			env["__GLX_VENDOR_LIBRARY_NAME"] = "nvidia"
			env["__NV_PRIME_RENDER_OFFLOAD"] = "1"
		}
		break
	}

	env["WINEDEBUG"] += ",warn+seh" // required to read Roblox logs
	env["XR_LOADER_DEBUG"] = "none" // already shown in Roblox log
	env["WINEDLLOVERRIDES"] += ";" + "dxdiagn,winemenubuilder.exe,mscoree,mshtml="

	// Studio's editor viewport keeps the cursor visible while orbiting
	// the camera with a mouse button held, so Wine's native Wayland
	// driver never engages its low-latency relative-motion path for
	// it (that path requires the cursor to be hidden); Studio instead
	// polls GetCursorPos once per frame and calls SetCursorPos to
	// recenter, which involves an async round trip to the compositor
	// under Wayland. At uneven or low frame rates that round trip
	// falls behind real mouse movement, which is felt as laggy or
	// suddenly "jumpy" camera control that gets worse the slower/more
	// uneven frames are (vinegarhq/vinegar#750, #766, #714, #851).
	// This has no effect outside Studio's editor (e.g. Play Solo,
	// which does hide the cursor) and no effect under X11/XWayland
	// sessions, where this variable is simply unused.
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		env["WINE_WAYLAND_EMULATE_MOUSE_WARP"] = "1"
	}

	if !c.Debug {
		env["WINEDEBUG"] += ",fixme-all,err-kerberos,err-ntlm,err-combase"
	}

	env["WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"] = "--disable-gpu"

	switch c.Studio.Renderer {
	case "DXVK", "DXVK-Sarek":
		env["WINE_D3D_CONFIG"] = "renderer=vulkan"
	case "Vulkan":
		env["VK_LOADER_LAYERS_ENABLE"] = "VK_LAYER_VINEGAR_VinegarLayer"
		env["WINE_D3D_CONFIG"] = "renderer=vulkan"
	}

	useDXVK := c.Studio.DXVKVersion() != ""

	if useDXVK {
		if !c.Debug {
			env["DXVK_LOG_LEVEL"] = "warn"
		}
		env["DXVK_LOG_PATH"] = "none"
		env["DXVK_STATE_CACHE_PATH"] = dirs.Cache

	}

	for k, v := range env {
		pfx.Env = append(pfx.Env, k+"="+v)
	}

	if useDXVK {
		dxvk.EnvOverride(pfx)
	}

	slog.Debug("Using Prefix environment", "env", pfx.Env)

	return pfx
}
