// Package deps pins modules used by the rendering, canvas, WebGL and media
// stacks so `go mod tidy` keeps them while those packages are being built.
package deps

import (
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/font"
	_ "golang.org/x/image/font/gofont/gobold"
	_ "golang.org/x/image/font/gofont/gobolditalic"
	_ "golang.org/x/image/font/gofont/goitalic"
	_ "golang.org/x/image/font/gofont/gomono"
	_ "golang.org/x/image/font/gofont/gomonobold"
	_ "golang.org/x/image/font/gofont/goregular"
	_ "golang.org/x/image/font/opentype"
	_ "golang.org/x/image/font/sfnt"
	_ "golang.org/x/image/math/fixed"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/vector"
	_ "golang.org/x/image/webp"
)
