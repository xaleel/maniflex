// Package jobsx bridges a scheduled.Runner to the jobs system, enabling the
// durable/distributed sweep path without making the core scheduled package
// depend on jobs.
//
// Wire one sweep per interval through jobs/cron so a SKIP LOCKED dequeue
// guarantees exactly one worker sweeps, even across a large replica fleet:
//
//	runner, _ := scheduled.New(server, cfg)
//	sched := cron.New(queue, logger)
//	sched.Add(cron.Entry{Every: time.Minute, Job: jobs.Job{Type: "maniflex.scheduled.sweep"}})
//	worker.Register("maniflex.scheduled.sweep", jobsx.JobHandler(runner))
package jobsx

import (
	"context"
	"encoding/json"

	"github.com/xaleel/maniflex/jobs"
	"github.com/xaleel/maniflex/scheduled"
)

// JobType is the conventional jobs.Job.Type for a scheduled sweep.
const JobType = "maniflex.scheduled.sweep"

// JobHandler returns a jobs.Handler that runs exactly one sweep. The Report is
// JSON-encoded into the Result's Output; a sweep error surfaces as the handler
// error so the jobs system can retry.
func JobHandler(r *scheduled.Runner) jobs.Handler {
	return func(ctx context.Context, _ jobs.Job) (jobs.Result, error) {
		rep, err := r.Sweep(ctx)
		if err != nil {
			return jobs.Result{}, err
		}
		out, err := json.Marshal(toReportJSON(rep))
		if err != nil {
			return jobs.Result{}, err
		}
		return jobs.Result{Output: out}, nil
	}
}

// reportJSON is a JSON-friendly projection of scheduled.Report (Report.Errors
// holds error values, which do not marshal usefully).
type reportJSON struct {
	Deleted  int                             `json:"deleted"`
	Updated  int                             `json:"updated"`
	PerModel map[string]scheduled.ModelCount `json:"per_model"`
	Errors   []string                        `json:"errors,omitempty"`
}

func toReportJSON(r scheduled.Report) reportJSON {
	rj := reportJSON{Deleted: r.Deleted, Updated: r.Updated, PerModel: r.PerModel}
	for _, e := range r.Errors {
		rj.Errors = append(rj.Errors, e.Error())
	}
	return rj
}
