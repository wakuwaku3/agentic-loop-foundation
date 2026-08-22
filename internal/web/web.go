// Package web contains the small owner console. It is deliberately server
// rendered and has no provider SDK or secret-bearing data path.
package web

import "embed"

//go:embed owner.html owner.css owner.js
var files embed.FS

func OwnerHTML() string                { b, _ := files.ReadFile("owner.html"); return string(b) }
func Asset(name string) ([]byte, bool) { b, err := files.ReadFile(name); return b, err == nil }
