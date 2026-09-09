package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"sigs.k8s.io/yaml"
)

// Theme is a skin: the colours every pane draws from. Colours are terminal
// colour values (an ANSI 256 index like "205", or a hex like "#ff8800"), which
// is what lipgloss accepts directly, so a skin file needs no colour parsing of
// its own.
type Theme struct {
	Name       string `json:"name"`
	Accent     string `json:"accent"`
	OK         string `json:"ok"`
	Warn       string `json:"warn"`
	Fail       string `json:"fail"`
	Muted      string `json:"muted"`
	Border     string `json:"border"`
	SelectedFG string `json:"selected_fg"`
	SelectedBG string `json:"selected_bg"`
}

// Built-in skins. Three, as planned: a dark default, a light one for pale
// terminals where 231-on-57 is unreadable, and k3s's own orange.
var builtinThemes = map[string]Theme{
	"dark": {
		Name: "dark", Accent: "205", OK: "42", Warn: "214", Fail: "196",
		Muted: "241", Border: "240", SelectedFG: "231", SelectedBG: "57",
	},
	"light": {
		Name: "light", Accent: "127", OK: "28", Warn: "130", Fail: "160",
		Muted: "243", Border: "250", SelectedFG: "231", SelectedBG: "62",
	},
	"k3s-orange": {
		Name: "k3s-orange", Accent: "208", OK: "40", Warn: "220", Fail: "203",
		Muted: "244", Border: "238", SelectedFG: "232", SelectedBG: "208",
	},
}

// ThemeNames lists the built-in skins, sorted.
func ThemeNames() []string {
	names := make([]string, 0, len(builtinThemes))
	for n := range builtinThemes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Styles derived from the active theme. They are package-level because one
// process draws with one skin: threading a style set through every render
// function would add a parameter to each of them and buy nothing.
var (
	accentColor lipgloss.Color
	selectedFg  lipgloss.Color
	selectedBg  lipgloss.Color

	titleStyle      lipgloss.Style
	helpStyle       lipgloss.Style
	errStyle        lipgloss.Style
	statusOKStyle   lipgloss.Style
	statusWarnStyle lipgloss.Style
	statusFailStyle lipgloss.Style
	paneBorder      lipgloss.Style
	podPrefixStyle  lipgloss.Style
	// YAML/describe highlighting.
	yamlKeyStyle     lipgloss.Style
	yamlValueStyle   lipgloss.Style
	yamlCommentStyle lipgloss.Style
	sectionStyle     lipgloss.Style
	// activeTheme is what the header reports and what tests assert against.
	activeTheme Theme
)

func init() { applyTheme(builtinThemes["dark"]) }

// applyTheme rebuilds every style from a skin.
func applyTheme(t Theme) {
	// A skin file may set only some fields; anything missing falls back to the
	// dark default rather than rendering as the terminal's default colour,
	// which would silently lose the ok/warn/fail distinction.
	d := builtinThemes["dark"]
	pick := func(v, fallback string) lipgloss.Color {
		if strings.TrimSpace(v) == "" {
			return lipgloss.Color(fallback)
		}
		return lipgloss.Color(v)
	}
	accentColor = pick(t.Accent, d.Accent)
	selectedFg = pick(t.SelectedFG, d.SelectedFG)
	selectedBg = pick(t.SelectedBG, d.SelectedBG)
	muted := pick(t.Muted, d.Muted)
	border := pick(t.Border, d.Border)

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accentColor).Padding(0, 1)
	helpStyle = lipgloss.NewStyle().Foreground(muted)
	errStyle = lipgloss.NewStyle().Foreground(pick(t.Fail, d.Fail)).Bold(true)
	statusOKStyle = lipgloss.NewStyle().Foreground(pick(t.OK, d.OK))
	statusWarnStyle = lipgloss.NewStyle().Foreground(pick(t.Warn, d.Warn))
	statusFailStyle = lipgloss.NewStyle().Foreground(pick(t.Fail, d.Fail))
	paneBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(0, 1)
	podPrefixStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("109"))
	yamlKeyStyle = lipgloss.NewStyle().Foreground(accentColor)
	yamlValueStyle = lipgloss.NewStyle().Foreground(pick(t.OK, d.OK))
	yamlCommentStyle = helpStyle
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(accentColor)

	activeTheme = t
	if activeTheme.Name == "" {
		activeTheme.Name = "custom"
	}
}

// SetTheme selects a skin by name, or loads one from a file if the name looks
// like a path. Unknown names are an error rather than a silent fallback: a
// typo would otherwise look like the skin simply had no effect.
func SetTheme(name string) error {
	if name == "" {
		return nil
	}
	if t, ok := builtinThemes[name]; ok {
		applyTheme(t)
		return nil
	}
	if looksLikePath(name) {
		t, err := LoadTheme(name)
		if err != nil {
			return err
		}
		applyTheme(t)
		return nil
	}
	// A bare name may also be a skin installed in the config directory.
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".config", "k3helper", "skins", name+".yaml")
		if _, err := os.Stat(path); err == nil {
			t, err := LoadTheme(path)
			if err != nil {
				return err
			}
			applyTheme(t)
			return nil
		}
	}
	return fmt.Errorf("unknown theme %q (built in: %s; or give a path to a skin YAML file)",
		name, strings.Join(ThemeNames(), ", "))
}

// LoadTheme reads a skin YAML file.
func LoadTheme(path string) (Theme, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Theme{}, fmt.Errorf("read theme: %w", err)
	}
	var t Theme
	if err := yaml.Unmarshal(data, &t); err != nil {
		return Theme{}, fmt.Errorf("parse theme %s: %w", path, err)
	}
	if t.Name == "" {
		t.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return t, nil
}

func looksLikePath(s string) bool {
	return strings.ContainsAny(s, "/\\") || strings.HasSuffix(s, ".yaml") || strings.HasSuffix(s, ".yml")
}

// --- syntax-aware rendering --------------------------------------------------

// highlightYAML colours keys, values and comments.
//
// Written here rather than pulled from a markdown renderer: the detail panes
// show YAML and `kubectl describe` output, neither of which is markdown, and a
// renderer would reflow the alignment that makes describe output readable.
func highlightYAML(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			b.WriteString("\n")
			continue
		case strings.HasPrefix(trimmed, "#"):
			b.WriteString(yamlCommentStyle.Render(line) + "\n")
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " -"))]
		rest := line[len(indent):]
		key, value, found := strings.Cut(rest, ":")
		if !found {
			b.WriteString(line + "\n")
			continue
		}
		out := indent + yamlKeyStyle.Render(key+":")
		if strings.TrimSpace(value) != "" {
			out += yamlValueStyle.Render(value)
		}
		b.WriteString(out + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// highlightDescribe bolds section headers and colours the Events table by
// event type, which is where the cause of a broken pod usually is.
func highlightDescribe(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		switch {
		case line == "":
			b.WriteString("\n")
		case !strings.HasPrefix(line, " ") && strings.HasSuffix(strings.TrimSpace(line), ":"):
			b.WriteString(sectionStyle.Render(line) + "\n")
		case strings.Contains(line, "Warning"):
			b.WriteString(statusWarnStyle.Render(line) + "\n")
		case strings.Contains(line, "Failed") || strings.Contains(line, "BackOff"):
			b.WriteString(statusFailStyle.Render(line) + "\n")
		default:
			b.WriteString(line + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- sparkline ---------------------------------------------------------------

// sparkBlocks are the eighth-height block glyphs, lowest to highest.
var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// sparkline renders samples (0-100) as a block-glyph graph.
//
// The scale is fixed to 0-100 rather than auto-scaled to the sample range:
// an auto-scaled graph of a flat 3% CPU looks identical to one of a flat 98%,
// which is exactly the distinction a dashboard exists to make.
func sparkline(samples []float64, width int) string {
	if width <= 0 || len(samples) == 0 {
		return ""
	}
	if len(samples) > width {
		samples = samples[len(samples)-width:]
	}
	var b strings.Builder
	for _, v := range samples {
		switch {
		case v < 0:
			v = 0
		case v > 100:
			v = 100
		}
		idx := int(v / 100 * float64(len(sparkBlocks)-1))
		style := statusOKStyle
		switch {
		case v >= 90:
			style = statusFailStyle
		case v >= 75:
			style = statusWarnStyle
		}
		b.WriteString(style.Render(string(sparkBlocks[idx])))
	}
	return b.String()
}
