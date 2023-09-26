//go:build tools
// +build tools

// This file will never be built, but `go mod tidy` will see the packages
// imported here as dependencies and not remove them from `go.mod`.

package tools

import (
	_ "github.com/arttor/helmify/cmd/helmify"
	_ "sigs.k8s.io/controller-runtime/tools/setup-envtest"
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
)
