package windows

import (
	"context"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/executor/joblog"
)

var _ core.JobInspector = (*Executor)(nil)

// JobLog implements core.JobInspector: the runner's diag logs, or a check
// job's log.txt, from the job's folder.
func (e *Executor) JobLog(ctx context.Context, name string, max int64) (core.JobLog, error) {
	if !core.NamePattern.MatchString(name) {
		return core.JobLog{}, core.ErrNotFound
	}
	return joblog.Read(e.jobDir(name), max)
}

// JobUsage implements core.JobInspector. The job object's accounting is
// not read yet, so Windows jobs report no usage.
func (e *Executor) JobUsage(ctx context.Context, name string) (core.Usage, error) {
	return core.Usage{}, nil
}
