package eventingest

import (
	"bytes"
	"io"
	"strings"
)

type bytesBuffer = bytes.Buffer

func stringsReader(s string) io.Reader { return strings.NewReader(s) }
