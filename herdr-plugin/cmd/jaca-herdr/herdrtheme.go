package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Herdr doesn't hand its theme to plugins, so the pane works it out the way Herdr does: the
// built-in theme named in config.toml's [theme] table (herdrthemes.go, copied from Herdr's
// source), with the colors of its [theme.custom] table on top. Popups then use what Herdr's own
// overlays use (its keybinds panel: panel_bg behind text, an accent border), and the log colors
// are the theme's.
//
// Read once at startup. [theme.custom.light] and [theme.custom.dark], and auto_switch, are not
// applied. A theme this build doesn't know leaves the terminal palette's colors.

// herdrConfigPath is Herdr's config file.
func herdrConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "herdr", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "config.toml")
}

// readHerdrTheme returns a Herdr config's theme name and its [theme.custom] colors (token ->
// color text). It reads the simple `key = "value"` lines those tables hold and skips the rest.
func readHerdrTheme(config string) (name string, custom map[string]string) {
	custom = map[string]string{}
	table := ""
	scanner := bufio.NewScanner(strings.NewReader(config))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			table = strings.TrimSpace(strings.Trim(strings.SplitN(line, "#", 2)[0], "[] \t"))
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || strings.HasPrefix(line, "#") || (table != "theme" && table != "theme.custom") {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if len(value) < 2 || (value[0] != '"' && value[0] != '\'') {
			continue
		}
		end := strings.IndexByte(value[1:], value[0])
		switch {
		case end < 0:
		case table == "theme.custom":
			custom[key] = value[1 : 1+end]
		case key == "name":
			name = value[1 : 1+end]
		}
	}
	return name, custom
}

// herdrThemeAliases are the other names Herdr accepts for a built-in theme
// (canonical_theme_name in its src/config/theme.rs).
var herdrThemeAliases = map[string]string{
	"catppuccin-mocha": "catppuccin", "latte": "catppuccin-latte", "light": "catppuccin-latte",
	"tokyonight": "tokyo-night", "tokyo-day": "tokyo-night-day", "tokyonight-day": "tokyo-night-day",
	"gruvbox-dark": "gruvbox", "onedark": "one-dark", "onelight": "one-light", "solarized-dark": "solarized",
	"lotus": "kanagawa-lotus", "rosepine": "rose-pine", "rosepine-dawn": "rose-pine-dawn", "dawn": "rose-pine-dawn",
}

// resolveHerdrTheme is the palette Herdr draws with for a config's theme name and custom colors:
// the built-in theme (catppuccin when none is named) with the custom colors on top. An unknown
// name, or "terminal", gives the custom colors alone.
func resolveHerdrTheme(name string, custom map[string]string) map[string]string {
	name = strings.NewReplacer(" ", "-", "_", "-").Replace(strings.ToLower(strings.TrimSpace(name)))
	if name == "" {
		name = "catppuccin"
	}
	if canonical, ok := herdrThemeAliases[name]; ok {
		name = canonical
	}
	theme := map[string]string{}
	for token, color := range herdrBuiltinThemes[name] {
		theme[token] = color
	}
	for token, color := range custom {
		theme[token] = color
	}
	return theme
}

// namedColors are the color names Herdr accepts, as SGR foreground codes.
var namedColors = map[string]int{
	"black": 30, "red": 31, "green": 32, "yellow": 33, "blue": 34, "magenta": 35, "cyan": 36, "gray": 37, "grey": 37,
	"darkgray": 90, "darkgrey": 90, "lightred": 91, "lightgreen": 92, "lightyellow": 93, "lightblue": 94,
	"lightmagenta": 95, "lightcyan": 96, "white": 97,
}

// colorParams turns a Herdr color (hex, rgb(r,g,b) or a name) into SGR parameters for the
// foreground, or the background when bg. ok is false for the reset aliases and for text that
// isn't a color, which leave the terminal's own color.
func colorParams(text string, bg bool) (params string, ok bool) {
	s := strings.ToLower(strings.TrimSpace(text))
	rgb := func(r, g, b int) (string, bool) {
		lead := 38
		if bg {
			lead = 48
		}
		return fmt.Sprintf("%d;2;%d;%d;%d", lead, r, g, b), true
	}
	switch {
	case strings.HasPrefix(s, "#") && len(s) == 7:
		if v, err := strconv.ParseUint(s[1:], 16, 32); err == nil {
			return rgb(int(v>>16), int(v>>8&0xff), int(v&0xff))
		}
	case strings.HasPrefix(s, "#") && len(s) == 4:
		if v, err := strconv.ParseUint(s[1:], 16, 16); err == nil {
			return rgb(int(v>>8)*17, int(v>>4&0xf)*17, int(v&0xf)*17)
		}
	case strings.HasPrefix(s, "rgb(") && strings.HasSuffix(s, ")"):
		parts := strings.Split(s[4:len(s)-1], ",")
		var n [3]int
		for i := 0; i < len(parts) && len(parts) == 3; i++ {
			v, err := strconv.Atoi(strings.TrimSpace(parts[i]))
			if err != nil || v < 0 || v > 255 {
				return "", false
			}
			n[i] = v
			if i == 2 {
				return rgb(n[0], n[1], n[2])
			}
		}
	default:
		if code, known := namedColors[s]; known {
			if bg {
				code += 10
			}
			return strconv.Itoa(code), true
		}
	}
	return "", false
}

// herdrTheme is the palette Herdr is drawing with, read once. Empty when its config can't be
// read: the panes then keep the terminal palette's colors.
var herdrTheme = func() map[string]string {
	data, err := os.ReadFile(herdrConfigPath())
	if err != nil {
		return nil
	}
	return resolveHerdrTheme(readHerdrTheme(string(data)))
}()

// themeFg is the SGR parameters for a theme token as a foreground, or the terminal palette's
// code given as fallback where the theme has no color for it.
func themeFg(theme map[string]string, token, fallback string) string {
	if params, ok := colorParams(theme[token], false); ok {
		return params
	}
	return fallback
}

// themePanel is the style a popup is filled with, as Herdr fills its own: the theme's text on
// its panel_bg. With no theme it is none, and the popup draws on the pane's default colors; a
// panel_bg set to a reset alias leaves the background alone, as it does in Herdr.
func themePanel(theme map[string]string) string {
	var params []string
	if text, ok := colorParams(theme["text"], false); ok {
		params = append(params, text)
	}
	if bg, ok := colorParams(theme["panel_bg"], true); ok {
		params = append(params, bg)
	}
	if len(params) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(params, ";") + "m"
}

// themeAccent is the style of a popup's border, the theme's accent as Herdr uses it, or none.
func themeAccent(theme map[string]string) string {
	if params, ok := colorParams(theme["accent"], false); ok {
		return "\x1b[" + params + "m"
	}
	return ""
}

// themeButton is the style of a popup's close button, as Herdr draws its " esc close ": bold, the
// panel's color on the accent. With no theme it is bold and reversed.
func themeButton(theme map[string]string) string {
	fg, okFg := colorParams(theme["panel_bg"], false)
	bg, okBg := colorParams(theme["accent"], true)
	if !okFg || !okBg {
		return "\x1b[1;7m"
	}
	return "\x1b[1;" + fg + ";" + bg + "m"
}
