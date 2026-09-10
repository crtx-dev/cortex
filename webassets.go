package cortex

import (
	"embed"
	"io/fs"
)

// PublicFS contains the Nift-generated Cortex frontend.
//
//go:embed public/* public/assets/css/* public/assets/images/* public/assets/js/*
var publicFS embed.FS

// PublicFS returns the generated public/ tree for the Cortex HTTP server.
func PublicFS() fs.FS {
	sub, err := fs.Sub(publicFS, "public")
	if err != nil {
		panic(err)
	}
	return sub
}
