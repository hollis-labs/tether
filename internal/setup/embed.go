package setup

import "embed"

// seedFS holds the embedded starter catalog tree. Files live under
// seed/catalog/ and are written to <dst>/catalog/ by WriteCatalog.
//
//go:embed seed
var seedFS embed.FS
