package design

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// tgz builds a gzipped tarball holding files, keyed by archive path.
func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}

	sort.Strings(names)

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, name := range names {
		body := files[name]

		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	return buf.Bytes()
}

// integrity returns the sha512 npm integrity string of b.
func integrity(b []byte) string {
	sum := sha512.Sum512(b)

	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}
