package main

import (
	"os"
	"runtime/debug"

	"github.com/SamuelSupe/git-rg/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.RunVersion(os.Args[1:], os.Stdout, os.Stderr, resolvedVersion()))
}

func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
