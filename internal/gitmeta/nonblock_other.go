//go:build !unix

package gitmeta

// openNonblock is 0 off unix: syscall.O_NONBLOCK isn't universally defined (e.g.
// js/wasm), and the cgo-free "build for any GOOS" invariant must hold. Those
// platforms have no blocking-fifo record-path concern in practice, so a plain
// blocking open is an acceptable, still-correct fallback.
const openNonblock = 0
