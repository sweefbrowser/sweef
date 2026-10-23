// Command pkgtool is the pure-Go helper behind Sweef's release packaging, so
// every installer can be assembled on any OS (including Linux CI) without
// macOS- or Windows-only tools:
//
//	lipo      build a universal (fat) Mach-O from thin binaries
//	lipo-info list the architectures in a Mach-O (thin or fat)
//	icns      write an Apple .icns container from PNGs
//	ico       write a Windows .ico container from PNGs
//	syso      write a Windows COFF .syso (manifest, version info, icon)
//	resinfo   dump the resources of a .syso / PE file (verification)
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"lipo": cmdLipo, "lipo-info": cmdLipoInfo, "icns": cmdIcns, "ico": cmdIco,
		"syso": cmdSyso, "resinfo": cmdResInfo,
	}
	fn, ok := cmds[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := fn(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "pkgtool %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: pkgtool <command> [args]
  lipo -o OUT THIN...
  lipo-info FILE
  icns -o OUT.icns PNG...            (PNG sizes 16..1024; size read from each file)
  ico  -o OUT.ico PNG...
  syso -arch amd64|arm64 -o OUT.syso -manifest FILE -ico FILE -version 0.1.0.7 [-name -desc -company -copyright -original -prerelease]
  resinfo FILE
           [-key-env UPDATE_SIGNING_KEY]  (signs to OUT.sig when the env var holds a base64 ed25519 seed)
`)
}
