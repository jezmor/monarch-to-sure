// Package web holds the wizard page shared by the Go and Python runtimes.
package web

import _ "embed"

//go:embed wizard.html
var WizardHTML []byte
