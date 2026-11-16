// Package worker exposes the standalone Worker source without a JavaScript build step.
package worker

import _ "embed"

// Template is the same source used by the offline Worker tests. Its single
// configuration marker is replaced with JSON only for a deliberate script export.
//
//go:embed worker.mjs
var Template string
