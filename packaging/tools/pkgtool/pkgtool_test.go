package main

import (
	"bytes"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testPNG(t *testing.T, n int) pngFile {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for i := 0; i < n; i++ {
		img.Set(i, i, color.RGBA{0x5b, 0x5b, 0xff, 0xff})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return pngFile{n, b.Bytes()}
}

// thinMachO fabricates a minimal 64-bit executable Mach-O header (no load commands).
func thinMachO(cpu, sub uint32, payload string) []byte {
	h := make([]byte, 32)
	le := binary.LittleEndian
	le.PutUint32(h[0:], 0xfeedfacf)
	le.PutUint32(h[4:], cpu)
	le.PutUint32(h[8:], sub)
	le.PutUint32(h[12:], 2) // MH_EXECUTE
	return append(h, payload...)
}

func TestLipoRoundTrip(t *testing.T) {
	amd := thinMachO(0x01000007, 3, strings.Repeat("A", 100))
	arm := thinMachO(0x0100000c, 0, strings.Repeat("B", 70000))
	a, err := parseThin("amd", amd)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseThin("arm", arm)
	if err != nil {
		t.Fatal(err)
	}
	fat, err := buildFat([]thin{b, a}) // order-independent
	if err != nil {
		t.Fatal(err)
	}
	ff, err := macho.NewFatFile(bytes.NewReader(fat))
	if err != nil {
		t.Fatal(err)
	}
	if len(ff.Arches) != 2 || ff.Arches[0].Cpu != macho.CpuAmd64 || ff.Arches[1].Cpu != macho.CpuArm64 {
		t.Fatalf("arches: %+v", ff.Arches)
	}
	if binary.BigEndian.Uint32(fat) != 0xcafebabe {
		t.Fatal("magic")
	}
	// slices intact and aligned
	for i, want := range [][]byte{amd, arm} {
		off := binary.BigEndian.Uint32(fat[8+20*i+8:])
		size := binary.BigEndian.Uint32(fat[8+20*i+12:])
		if off%(1<<14) != 0 {
			t.Errorf("slice %d offset %d not 16K aligned", i, off)
		}
		if !bytes.Equal(fat[off:off+size], want) {
			t.Errorf("slice %d corrupted", i)
		}
	}
	fat2, _ := buildFat([]thin{a, b})
	if !bytes.Equal(fat, fat2) {
		t.Fatal("not deterministic")
	}
	if _, err := buildFat([]thin{a, a}); err == nil {
		t.Fatal("duplicate arch must fail")
	}
	if _, err := parseThin("junk", []byte("not macho")); err == nil {
		t.Fatal("junk must fail")
	}
}

func TestICNS(t *testing.T) {
	b, err := buildICNS([]pngFile{testPNG(t, 16), testPNG(t, 32), testPNG(t, 512)})
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:4]) != "icns" || int(binary.BigEndian.Uint32(b[4:])) != len(b) {
		t.Fatal("header/length")
	}
	var types []string
	for off := 8; off < len(b); {
		l := int(binary.BigEndian.Uint32(b[off+4:]))
		if l < 8 || off+l > len(b) {
			t.Fatalf("bad entry length at %d", off)
		}
		if !bytes.HasPrefix(b[off+8:], []byte("\x89PNG")) {
			t.Fatalf("entry %s is not PNG", b[off:off+4])
		}
		types = append(types, string(b[off:off+4]))
		off += l
	}
	if strings.Join(types, ",") != "icp4,icp5,ic11,ic09,ic14" {
		t.Fatalf("types %v", types)
	}
	if _, err := buildICNS([]pngFile{testPNG(t, 20)}); err == nil {
		t.Fatal("unsupported size must fail")
	}
}

func TestICO(t *testing.T) {
	b, err := buildICO([]pngFile{testPNG(t, 16), testPNG(t, 256)})
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(b[2:]) != 1 || binary.LittleEndian.Uint16(b[4:]) != 2 {
		t.Fatal("dir")
	}
	if b[6] != 16 || b[6+16] != 0 {
		t.Fatalf("width bytes %d %d", b[6], b[6+16])
	}
	off := binary.LittleEndian.Uint32(b[6+16+12:])
	if !bytes.HasPrefix(b[off:], []byte("\x89PNG")) {
		t.Fatal("payload offset")
	}
	if _, err := buildICO([]pngFile{testPNG(t, 512)}); err == nil {
		t.Fatal(">256 must fail")
	}
}

func buildTestSyso(t *testing.T, arch string) (string, []byte) {
	t.Helper()
	ico, _ := buildICO([]pngFile{testPNG(t, 16), testPNG(t, 48), testPNG(t, 256)})
	ir, err := buildIconResources(ico)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`<?xml version="1.0"?><assembly manifestVersion="1.0"/>`)
	res := append([]resource{{rtManifest, 1, langEnUS, manifest}}, ir...)
	res = append(res, resource{rtVersion, 1, langEnUS, buildVersionInfo(versionInfo{
		version: [4]uint16{0, 1, 0, 7}, prerelease: true, name: "Sweef", desc: "Sweef Browser",
		company: "Sweef", original: "sweef.exe", ver: "0.1.0-alpha.7"})})
	b, err := buildSyso(arch, res)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "rsrc.syso")
	os.WriteFile(p, b, 0o644)
	return p, manifest
}

func TestSysoStructure(t *testing.T) {
	for arch, machine := range map[string]uint16{"amd64": pe.IMAGE_FILE_MACHINE_AMD64, "arm64": pe.IMAGE_FILE_MACHINE_ARM64} {
		p, manifest := buildTestSyso(t, arch)
		f, err := pe.Open(p)
		if err != nil {
			t.Fatalf("%s: debug/pe: %v", arch, err)
		}
		if f.Machine != machine || len(f.Sections) != 1 || f.Sections[0].Name != ".rsrc" {
			t.Fatalf("%s: header %+v", arch, f.FileHeader)
		}
		if int(f.Sections[0].NumberOfRelocations) != 6 { // manifest + 3 icons + group + version
			t.Fatalf("%s: relocs %d", arch, f.Sections[0].NumberOfRelocations)
		}
		f.Close()
		rs, err := readResources(p)
		if err != nil {
			t.Fatal(err)
		}
		got := map[uint32]int{}
		for _, r := range rs {
			got[r.typ]++
			if r.typ == rtManifest && !bytes.Equal(r.data, manifest) {
				t.Error("manifest payload mismatch")
			}
			if r.typ == rtVersion {
				kv := map[string]string{}
				for _, p := range versionStrings(r.data) {
					kv[p[0]] = p[1]
				}
				if kv["ProductVersion"] != "0.1.0-alpha.7" || kv["ProductName"] != "Sweef" || kv["OriginalFilename"] != "sweef.exe" {
					t.Errorf("version strings %v", kv)
				}
				if !bytes.Contains(r.data[:64], []byte{0xBD, 0x04, 0xEF, 0xFE}) {
					t.Error("VS_FIXEDFILEINFO signature missing")
				}
			}
		}
		if got[rtManifest] != 1 || got[rtIcon] != 3 || got[rtGroupIcon] != 1 || got[rtVersion] != 1 {
			t.Errorf("%s: resource counts %v", arch, got)
		}
	}
}

// TestSysoLinks proves the Go linker accepts the object and emits a valid .rsrc.
func TestSysoLinks(t *testing.T) {
	if testing.Short() {
		t.Skip("links a windows binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		p, manifest := buildTestSyso(t, arch)
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.20\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
		b, _ := os.ReadFile(p)
		os.WriteFile(filepath.Join(dir, "rsrc_windows_"+arch+".syso"), b, 0o644)
		exe := filepath.Join(dir, "x.exe")
		cmd := exec.Command(goBin, "build", "-o", exe, ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s link: %v\n%s", arch, err, out)
		}
		rs, err := readResources(exe)
		if err != nil {
			t.Fatalf("%s: linked exe resources: %v", arch, err)
		}
		found := false
		for _, r := range rs {
			if r.typ == rtManifest && bytes.Equal(r.data, manifest) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: manifest not found in linked exe (relocations wrong?)", arch)
		}
	}
}
