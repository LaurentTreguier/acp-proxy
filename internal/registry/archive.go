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
// escaping dst are rejected. Tar archives support symlinks and hardlinks,
// which packaging tools (e.g. PyInstaller) rely on.
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
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
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
		case tar.TypeSymlink:
			if err := makeSymlink(dst, target, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeLink:
			source, err := safeJoin(dst, hdr.Linkname)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Link(source, target); err != nil {
				// Fall back to a copy when the link source is missing
				// (some archives order hardlinks before their target).
				if copyFile(source, target) != nil {
					return fmt.Errorf("hardlink %q -> %q: %w", hdr.Name, hdr.Linkname, err)
				}
			}
		default:
			// Skip devices, fifos, etc.
		}
	}
}

// makeSymlink creates a symlink at target whose link text is linkname.
// Absolute link targets and targets escaping dst are rejected: archives
// may not plant links pointing outside the install directory.
func makeSymlink(dst, target, linkname string) error {
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("symlink %q has absolute target %q", target, linkname)
	}
	// The link resolves relative to its own directory; it must stay
	// inside the install directory.
	resolved := filepath.Clean(filepath.Join(filepath.Dir(target), linkname))
	rel, err := filepath.Rel(dst, resolved)
	if err != nil || rel == ".." || (len(rel) >= 2 && rel[:2] == "..") {
		return fmt.Errorf("symlink %q target %q escapes install directory", target, linkname)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	_ = os.Remove(target)
	return os.Symlink(linkname, target)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
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
