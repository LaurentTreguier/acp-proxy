package registry

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extract unpacks the archive at src into dst. Supported formats: .tar.gz
// and .zip, detected from the file name. Archive members with paths
// escaping dst are rejected.
func extract(src, dst string) error {
	if strings.HasSuffix(src, ".zip") {
		return extractZip(src, dst)
	}
	if strings.HasSuffix(src, ".tar.gz") || strings.HasSuffix(src, ".tgz") {
		return extractTarGz(src, dst)
	}
	return fmt.Errorf("unsupported archive %s", src)
}

func extractZip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		target, err := safeJoin(dst, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(target, rc, f.Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			continue
		}
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(hdr.Mode)
		if mode == 0 {
			mode = 0o755
		}
		if err := writeFile(target, tr, mode); err != nil {
			return err
		}
	}
}

// safeJoin joins name under dir, rejecting absolute paths and traversal.
func safeJoin(dir, name string) (string, error) {
	name = filepath.Clean("/" + name) // strips ./, ../ and leading slashes
	target := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("archive path %q escapes destination", name)
	}
	return target, nil
}

func writeFile(target string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
