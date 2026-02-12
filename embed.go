package main

import "embed"

//go:embed defaults/themes/*.toml
//go:embed defaults/pricing/*.toml
//go:embed defaults/forecasters/*.toml
//go:embed defaults/widgets/*.html
//go:embed defaults/vendor/*
//go:embed defaults/runtime.html
var defaultPlugins embed.FS
