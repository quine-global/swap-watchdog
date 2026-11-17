package main

// Styling with CSS: GTK3's own (https://docs.gtk.org/gtk3/css-overview.html,
// selectors in https://docs.gtk.org/gtk3/css-properties.html). Built-in
// defaults first, then /etc/force-quit-monitor/style.css (the system's, e.g.
// a distribution's), then the user's $XDG_CONFIG_HOME/force-quit-monitor/
// style.css (~/.config/...): a later one wins. Both files are read again when
// they change, at the next refresh (every 2 s), and a file that's removed
// stops applying. What to select is in the README ("Styling").

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/gtk"
)

const systemStylePath = "/etc/force-quit-monitor/style.css"

// defaultCSS is how the headline looked when it was Pango markup: the
// stylesheets can restyle it, which inline markup wouldn't allow.
const defaultCSS = `
#headline { font-weight: bold; font-size: large; }
#headline.alarm { color: red; font-size: x-large; }
`

func userStylePath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "force-quit-monitor", "style.css")
}

type styleFile struct {
	path     string
	priority uint
	provider *gtk.CssProvider
	loaded   bool
	mtime    time.Time
	problem  string
}

// setupStyles installs the default style and the two files' providers on
// the default screen, and returns a function that (re)loads whichever files
// changed and reports the ones that didn't parse.
func setupStyles() func() []string {
	screen, err := gdk.ScreenGetDefault()
	if err != nil {
		log.Printf("no screen to style: %v", err)
		return func() []string { return nil }
	}
	base, err := gtk.CssProviderNew()
	if err == nil {
		if err := base.LoadFromData(defaultCSS); err != nil {
			log.Printf("built-in style: %v", err)
		}
		gtk.AddProviderForScreen(screen, base, uint(gtk.STYLE_PROVIDER_PRIORITY_APPLICATION))
	}
	files := []*styleFile{
		{path: systemStylePath, priority: uint(gtk.STYLE_PROVIDER_PRIORITY_APPLICATION) + 1},
		{path: userStylePath(), priority: uint(gtk.STYLE_PROVIDER_PRIORITY_USER)},
	}
	for _, f := range files {
		if f.provider, err = gtk.CssProviderNew(); err != nil {
			log.Printf("style %s: %v", f.path, err)
			continue
		}
		gtk.AddProviderForScreen(screen, f.provider, f.priority)
	}

	return func() []string {
		var problems []string
		for _, f := range files {
			if f.provider == nil {
				continue
			}
			info, err := os.Stat(f.path)
			if err != nil {
				if f.loaded {
					_ = f.provider.LoadFromData("")
					f.loaded, f.mtime, f.problem = false, time.Time{}, ""
				}
				continue
			}
			if !f.loaded || !info.ModTime().Equal(f.mtime) {
				f.loaded, f.mtime, f.problem = true, info.ModTime(), ""
				if err := f.provider.LoadFromPath(f.path); err != nil {
					f.problem = fmt.Sprintf("%s: %v", f.path, err)
					log.Print(f.problem)
				}
			}
			if f.problem != "" {
				problems = append(problems, f.problem)
			}
		}
		return problems
	}
}
