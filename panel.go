// panel.go carries the browser UI for the scheduler rules. It is served as an
// unauthenticated plugin resource; everything it displays it fetches with the
// operator's own management key, so the page itself holds no secrets and needs
// no build step.
package main

import _ "embed"

// panelHTML is the page served at /v0/resource/plugins/staged-scheduler/panel.
//
//go:embed panel.html
var panelHTML string
