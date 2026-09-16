package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func run() (err error) {
	if len(os.Args) != 4 {
		return errors.New("uso: zip-release DIRECTORY_SORGENTE ARCHIVIO_ZIP SOURCE_DATE_EPOCH")
	}
	source, err := filepath.Abs(os.Args[1])
	if err != nil {
		return err
	}
	epoch, err := strconv.ParseInt(os.Args[3], 10, 64)
	if err != nil || epoch < 0 {
		return errors.New("SOURCE_DATE_EPOCH non valido")
	}
	var paths []string
	if err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != source {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Strings(paths)

	output, err := os.OpenFile(os.Args[2], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = output.Close()
		if !keep {
			_ = os.Remove(os.Args[2])
		}
	}()
	zw := zip.NewWriter(output)
	base := filepath.Dir(source)
	modified := time.Unix(epoch, 0).UTC()
	for _, path := range paths {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("tipo di file non supportato nello ZIP: %s", path)
		}
		name, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return relErr
		}
		name = filepath.ToSlash(name)
		if info.IsDir() {
			name += "/"
		}
		header, headerErr := zip.FileInfoHeader(info)
		if headerErr != nil {
			return headerErr
		}
		header.Name = strings.TrimPrefix(name, "./")
		header.Modified = modified
		header.Method = zip.Deflate
		writer, createErr := zw.CreateHeader(header)
		if createErr != nil {
			return createErr
		}
		if info.IsDir() {
			continue
		}
		input, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		_, copyErr := io.Copy(writer, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = output.Sync(); err != nil {
		return err
	}
	if err = output.Close(); err != nil {
		return err
	}
	keep = true
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Errore:", err)
		os.Exit(1)
	}
}
