package server

import (
	"embed"
	"fmt"
	"html/template"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

func loadTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"statusColor": func(s string) string {
			switch s {
			case "running":
				return "bg-emerald-500/15 text-emerald-400 ring-emerald-500/30"
			case "creating", "start", "restart":
				return "bg-amber-500/15 text-amber-400 ring-amber-500/30"
			case "stop", "exited":
				return "bg-slate-500/15 text-slate-400 ring-slate-500/30"
			default:
				return "bg-rose-500/15 text-rose-400 ring-rose-500/30"
			}
		},
		"typeIcon": func(t string) string {
			switch t {
			case "redis":
				return "⚡"
			case "postgres":
				return "🐘"
			case "sqlite":
				return "🗃️"
			default:
				return "📦"
			}
		},
		"timeFmt": func(t time.Time) string {
			return t.Format("2006-01-02 15:04")
		},
		"upper":  strings.ToUpper,
		"initial": func(s string) string {
			if s == "" {
				return "?"
			}
			for _, r := range s {
				return string(r)
			}
			return "?"
		},
	}

	pages := []string{"login", "dashboard", "instance"}
	out := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t, err := template.New("layout.html").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("解析模板 %s: %w", p, err)
		}
		out[p] = t
	}
	return out, nil
}
