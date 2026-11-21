package darwin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/baney75/tentmaker/internal/core"
)

// LogTailBytes is how much of log.txt CheckResult returns.
const LogTailBytes = 64 << 10

const maxResultBytes = 1 << 20

// readCheckResult reads <rel>/result.json and the tail of <rel>/log.txt
// under base. base is trusted (the daemon's own); what is under it came
// from the job, so the files are opened through an os.Root (no symlink leaves
// base), must be regular files, and are read with a size cap. Opening with
// O_NONBLOCK keeps a FIFO the job planted from hanging the daemon.
func readCheckResult(base, rel, name string) core.CheckResult {
	res := core.CheckResult{Runner: name, State: "error"}
	root, err := os.OpenRoot(base)
	if err != nil {
		res.Description = "can't open the job's output: " + err.Error()
		return res
	}
	defer root.Close()
	logTail, logErr := readTail(root, rel+"/log.txt", LogTailBytes)
	res.LogTail = logTail
	data, err := readCapped(root, rel+"/result.json", maxResultBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Description = "check-run wrote no result.json"
		} else {
			res.Description = "can't read result.json: " + err.Error()
		}
		if logErr != nil && !errors.Is(logErr, os.ErrNotExist) {
			res.Description += "; log.txt: " + logErr.Error()
		}
		return res
	}
	var got core.CheckResult
	if err := json.Unmarshal(data, &got); err != nil {
		res.Description = "result.json is not valid JSON: " + err.Error()
		return res
	}
	switch got.State {
	case "success", "failure", "error":
	default:
		res.Description = fmt.Sprintf("result.json has state %q; want success, failure or error", trunc(got.State, 40))
		return res
	}
	got.Runner = name
	got.LogTail = logTail
	got.Description = trunc(got.Description, 1000)
	return got
}

func openRegular(root *os.Root, rel string) (*os.File, os.FileInfo, error) {
	st, err := root.Lstat(rel)
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", rel)
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return f, st, nil
}

func readCapped(root *os.Root, rel string, max int64) ([]byte, error) {
	f, _, err := openRegular(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is over %d bytes", rel, max)
	}
	return data, nil
}

func readTail(root *os.Root, rel string, n int64) (string, error) {
	f, st, err := openRegular(root, rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	off := st.Size() - n
	if off < 0 {
		off = 0
	}
	data, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, off, n), n))
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(data), "?"), nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
