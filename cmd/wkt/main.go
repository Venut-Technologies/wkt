package main

import (
	"os"
	"runtime/debug"

	"github.com/Venut-Technologies/wkt/internal/cli"
)

// version is stamped by the release build with
// -ldflags "-X main.version=v0.1.0". A "go install module@version" build gets
// no ldflags at all, so fall back to the version the toolchain recorded — a
// binary that reports "dev" when it was installed from a tag makes every bug
// report start with a guess.
var version = "dev"

func main() {
	cli.Version = resolveVersion()
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}

func resolveVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return version
	}
	return info.Main.Version
}
