package docker

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"testing"
)

const fuzzArchiveMaximum = 1024

func FuzzReadSingleRegularArchive(f *testing.F) {
	f.Add(fuzzTarArchive(tar.TypeReg, []byte("resource evidence"), false))
	f.Add(fuzzTarArchive(tar.TypeSymlink, nil, false))
	f.Add(fuzzTarArchive(tar.TypeReg, []byte("one"), true))
	f.Add(fuzzTarArchive(tar.TypeReg, bytes.Repeat([]byte{'x'}, fuzzArchiveMaximum+1), false))
	f.Add([]byte("not a tar archive"))

	f.Fuzz(func(t *testing.T, input []byte) {
		content, err := readSingleRegularArchive(bytes.NewReader(input), fuzzArchiveMaximum)
		if err != nil {
			return
		}
		if len(content) < 1 || len(content) > fuzzArchiveMaximum {
			t.Fatalf("accepted content length %d outside 1..%d", len(content), fuzzArchiveMaximum)
		}

		reader := tar.NewReader(bytes.NewReader(input))
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("accepted archive whose first header cannot be read: %v", err)
		}
		if header.Typeflag != tar.TypeReg {
			t.Fatalf("accepted non-regular archive entry type %d", header.Typeflag)
		}
		if header.Size != int64(len(content)) {
			t.Fatalf("accepted header size %d but returned %d bytes", header.Size, len(content))
		}
		reference, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read accepted archive content independently: %v", err)
		}
		if !bytes.Equal(reference, content) {
			t.Fatal("accepted archive content differs from independently decoded entry")
		}
		if _, err := reader.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("accepted archive with another entry: %v", err)
		}
	})
}

func fuzzTarArchive(entryType byte, content []byte, second bool) []byte {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	header := &tar.Header{Name: "evidence.json", Mode: 0o600, Size: int64(len(content)), Typeflag: entryType}
	if entryType == tar.TypeSymlink {
		header.Size = 0
		header.Linkname = "elsewhere"
	}
	_ = writer.WriteHeader(header)
	if entryType == tar.TypeReg {
		_, _ = writer.Write(content)
	}
	if second {
		_ = writer.WriteHeader(&tar.Header{Name: "second", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
		_, _ = writer.Write([]byte{'x'})
	}
	_ = writer.Close()
	return buffer.Bytes()
}
