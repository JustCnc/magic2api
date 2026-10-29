//go:build !cgo

package main

import (
	"encoding/json"
	"errors"
)

// The C ABI only exists in a c-shared build. This stub keeps
// `CGO_ENABLED=0 go test ./...` and `go vet` working on hosts without a C
// toolchain; the resulting binary is a plain executable, not a plugin.
func main() {}

var errNoHost = errors.New("plugin ABI unavailable: build with CGO_ENABLED=1 and -buildmode=c-shared")

var hostCall = func(method string, payload any) (json.RawMessage, error) {
	return nil, errNoHost
}
