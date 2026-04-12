package mailer

import (
	"embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/*.html templates/*.txt
var templateFS embed.FS

// htmlTemplates parses all HTML templates. Each file is self-contained
// (includes its own full HTML document) — no shared layout inheritance.
// This is simpler than cross-file define/template blocks and there are
// only a handful of templates.
var htmlTemplates = htmltemplate.Must(htmltemplate.ParseFS(templateFS, "templates/*.html"))
var textTemplates = texttemplate.Must(texttemplate.ParseFS(templateFS, "templates/*.txt"))
