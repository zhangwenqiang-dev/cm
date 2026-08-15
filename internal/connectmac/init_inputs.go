package connectmac

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/term"
)

func readInitSecret(prompt string, in io.Reader, out io.Writer) (string, error) {
	if in == nil {
		return "", fmt.Errorf("read secret: input is nil")
	}
	if out != nil {
		fmt.Fprint(out, prompt)
	}
	if inputFile, ok := in.(*os.File); ok && term.IsTerminal(int(inputFile.Fd())) {
		secret, err := term.ReadPassword(int(inputFile.Fd()))
		if out != nil {
			fmt.Fprintln(out)
		}
		if err != nil {
			return "", fmt.Errorf("read secret: %w", err)
		}
		return strings.TrimSpace(string(secret)), nil
	}
	if out != nil {
		fmt.Fprintln(out, "warning: input is not a terminal; echo cannot be disabled")
	}
	secret, err := readInitInputLine(in)
	if err != nil && len(secret) == 0 {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return strings.TrimSpace(secret), nil
}

func readInitInputLine(in io.Reader) (string, error) {
	var line strings.Builder
	var next [1]byte
	for {
		n, err := in.Read(next[:])
		if n > 0 {
			line.WriteByte(next[0])
			if next[0] == '\n' {
				return line.String(), nil
			}
		}
		if err != nil {
			return line.String(), err
		}
		if n == 0 {
			return line.String(), io.ErrNoProgress
		}
	}
}

func discoverInitPEMFiles(sshDir string) ([]string, error) {
	entries, err := os.ReadDir(sshDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("read SSH directory: %w", err)
	}
	files := make([]string, 0)
	for _, entry := range entries {
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".pem") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect SSH key %s: %w", entry.Name(), err)
		}
		if info.Mode().IsRegular() {
			files = append(files, filepath.Join("~/.ssh", entry.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}
