package webassets

import "embed"

//go:embed templates/* static/* public_templates/* public_static/*
var Files embed.FS
