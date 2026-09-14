// Package data embeds the example config template that lives alongside it in
// this directory. The runtime Docker image copies in only the compiled
// binary (see ../Dockerfile — the final stage never COPYs from data/), so
// without this embed a fresh deployment would have no template on disk to
// write out a starting config.yaml from. See config.EnsureFile, called from
// cmd/bot.
package data

import _ "embed"

//go:embed config.example.yaml
var ExampleConfigYAML []byte
