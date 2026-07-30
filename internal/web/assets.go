package web

import (
	"embed"
	"io/fs"
)

//go:embed templates/*.html
var embeddedTemplates embed.FS

//go:embed static/*
var embeddedStatic embed.FS
var templateFiles = embeddedTemplates
var staticFiles, _ = fs.Sub(embeddedStatic, "static")
