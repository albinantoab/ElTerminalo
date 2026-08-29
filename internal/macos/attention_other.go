//go:build !darwin

package macos

// The app only ships for macOS. This file exists so that `go vet`, `go build`
// and an editor's language server still work when they are pointed at another
// GOOS — the cgo half in attention_darwin.go is unbuildable there, and a
// package that cannot be type-checked cross-platform makes every tool that
// walks ./... noisy.

// Beep plays the system alert sound. Nothing to play off macOS.
func Beep() {}

// RequestAttention bounces the Dock icon. There is no Dock off macOS.
func RequestAttention() {}
