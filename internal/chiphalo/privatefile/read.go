// Package privatefile reads bounded local migration inputs without following
// symlinks or accepting a file replaced between inspection and opening.
package privatefile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
)

func Read(path string, limit int64, secret bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("private input is missing or is not a regular file")
	}
	if secret && runtime.GOOS != "windows" && info.Mode().Perm()&0027 != 0 {
		return nil, fmt.Errorf("private input permissions allow unauthorized access")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open private input")
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("private input changed while opening")
	}
	if opened.Size() > limit || limit <= 0 {
		return nil, fmt.Errorf("private input exceeds byte limit")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, fmt.Errorf("cannot read bounded private input")
	}
	return raw, nil
}

func Decode(raw []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("invalid private JSON input")
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("trailing data in private JSON input")
	}
	return nil
}
