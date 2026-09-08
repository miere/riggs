package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Riggs' own schedule, stored beside the notification ledger.
//
// It is here rather than in `config.yaml` because a job is not a setting. Half
// of every record is what happened — when it last ran, for how long, whether it
// worked, and what it said when it did not — and that has no place in a
// hand-edited file whose comments are the reason it can be fixed. The
// definition and its outcome are one row because the Home tab draws them as one
// line, and splitting them would mean two reads and a join to render it.
//
// It is in this package, alongside `items` and `http_cache`, for the reason
// those are: this package owns the database, and a second package opening the
// same file would own its schema jointly with this one.

// Job is one scheduled invocation of the riggs binary, and the outcome of its
// last run.
type Job struct {
	// Name is the job's identity. It is the primary key, and it rides in a
	// Slack block_id on the Home tab, so it is constrained (see
	// schedule.ValidateName) rather than free text.
	Name string
	// Type is what KIND of job this is, and it decides everything else about
	// it: which parameters Params carries, which modal edits it, which timeout
	// applies, and what argument list it is eventually run as.
	//
	// The ledger never interprets it — it is an opaque domain label here, the
	// same way a card's State is (§9c). The vocabulary is schedule.Kind's, and
	// a row carrying a kind this build does not know is a row this build will
	// not run, which is the point: a type it cannot render arguments for is
	// better refused than guessed at.
	Type string
	// Params is the job's own configuration, bespoke to its Type: a GitHub
	// login for the review digest, a JQL query for the ticket one.
	//
	// A map rather than a struct per kind because this table stores it as one
	// JSON column and has no business knowing what the keys mean. The typed
	// reading of it — which keys are required, what they are called, what a
	// missing one means — belongs to schedule, which owns the kinds.
	Params map[string]string
	// Spec is the schedule as written: "3m", or "0 9 * * 1-5".
	Spec string
	// Enabled is whether the scheduler will fire it. A disabled job keeps its
	// definition and its history: "off for now" and "deleted" are different
	// intentions, and only one of them is recoverable.
	Enabled bool

	// LegacyArgs is the argument list a build before typed jobs stored, and it
	// is the ONLY thing that still reads the `args` column.
	//
	// It exists for the migration and for nothing else: a job written by an
	// older Riggs has no type and no params, and the only description of what
	// it did is the argv it was going to run. schedule.Adopt reads it back into
	// a Type and a Params, after which the column is written empty and this
	// field stays that way. Nothing in the running system may reach for it — a
	// job's arguments are DERIVED from its type now (schedule.Args), and a
	// second source for them is how the two would come apart.
	LegacyArgs []string

	CreatedAt time.Time
	UpdatedAt time.Time

	// LastRun is zero until the job has run once.
	LastRun time.Time
	// LastOK reports whether that run succeeded.
	LastOK bool
	// LastDuration is how long it took.
	LastDuration time.Duration
	// LastError is why it failed, empty on success.
	LastError string
	// LastOutput is the tail of what it printed, kept only for a failure — a
	// successful job's chatter is not worth the rows.
	LastOutput string
}

// Ran reports whether the job has ever run.
func (j Job) Ran() bool { return !j.LastRun.IsZero() }

// Jobs lists every job, by name.
//
// Ordered so the Home tab, the CLI and two consecutive reads all agree: a list
// whose order moves under the reader is one they cannot scan.
func (s *Store) Jobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, jobColumns+` FROM jobs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("notify: listing jobs: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("notify: reading a job: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// Job returns one job by name.
func (s *Store) Job(ctx context.Context, name string) (Job, bool, error) {
	row := s.db.QueryRowContext(ctx, jobColumns+` FROM jobs WHERE name = ?`, name)
	job, err := scanJob(row)
	if err == sql.ErrNoRows {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("notify: reading job %s: %w", name, err)
	}
	return job, true, nil
}

// SaveJob inserts or updates a job's DEFINITION, leaving its history alone.
//
// The run columns are deliberately not in the UPDATE. Editing a schedule is not
// a statement about whether the last run worked, and blanking that would take
// the one piece of evidence anybody has when a job starts failing after a
// change — which is exactly when they would look.
func (s *Store) SaveJob(ctx context.Context, job Job) error {
	params, err := json.Marshal(job.Params)
	if err != nil {
		return fmt.Errorf("notify: encoding the parameters of job %s: %w", job.Name, err)
	}
	// The legacy column is written back EMPTY, always. A typed job's arguments
	// are derived from its type and its parameters; leaving the old argv beside
	// them would be a second answer to "what does this run", and the migration
	// would find it again on the next start and try to adopt an already-adopted
	// row.
	args, err := json.Marshal([]string{})
	if err != nil {
		return fmt.Errorf("notify: encoding the arguments of job %s: %w", job.Name, err)
	}
	now := job.UpdatedAt
	if now.IsZero() {
		now = time.Now()
	}
	created := job.CreatedAt
	if created.IsZero() {
		created = now
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO jobs (name, type, params, args, spec, timeout_ms, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   type = excluded.type, params = excluded.params, args = excluded.args,
		   spec = excluded.spec, timeout_ms = 0, enabled = excluded.enabled,
		   updated_at = excluded.updated_at`,
		job.Name, job.Type, string(params), string(args), job.Spec, boolToInt(job.Enabled),
		created.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("notify: saving job %s: %w", job.Name, err)
	}
	return nil
}

// SetJobEnabled turns one job on or off without touching anything else.
//
// found is false for a name that is not there, which is a Home tab published
// before somebody deleted the job — worth telling apart from a write that
// silently did nothing.
func (s *Store) SetJobEnabled(ctx context.Context, name string, enabled bool, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET enabled = ?, updated_at = ? WHERE name = ?`,
		boolToInt(enabled), at.UTC().Format(time.RFC3339), name)
	if err != nil {
		return false, fmt.Errorf("notify: setting job %s enabled=%v: %w", name, enabled, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteJob forgets a job and its history.
func (s *Store) DeleteJob(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("notify: deleting job %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RecordJobRun writes the outcome of one run.
//
// Only the last one is kept. A full history belongs in a log, and the question
// this table answers — "is this job working?" — is answered by the most recent
// answer to it. Keeping every run would also mean deciding when to prune, on a
// database whose whole design is that it never grows without bound.
//
// output is stored as given. Whether a successful run's chatter is worth
// keeping is the scheduler's call, not this table's.
func (s *Store) RecordJobRun(ctx context.Context, name string, at time.Time,
	took time.Duration, runErr error, output string) error {

	failure := ""
	if runErr != nil {
		failure = runErr.Error()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET last_run_at = ?, last_ok = ?, last_ms = ?, last_error = ?, last_output = ?
		   WHERE name = ?`,
		at.UTC().Format(time.RFC3339), boolToInt(runErr == nil), took.Milliseconds(),
		failure, output, name)
	if err != nil {
		return fmt.Errorf("notify: recording the run of job %s: %w", name, err)
	}
	return nil
}

// jobColumns is the SELECT list every read shares, so scanJob can serve them
// all and a column added in one place cannot be forgotten in another.
const jobColumns = `SELECT name, type, params, args, spec, enabled, created_at, updated_at,
	last_run_at, last_ok, last_ms, last_error, last_output`

// scanner is the shared shape of *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// scanJob reads one row.
func scanJob(row scanner) (Job, error) {
	var (
		job                                     Job
		args, params, created, updated, lastRun string
		enabled, lastOK                         int
		lastMS                                  int64
		lastError, lastOutput, spec, jname      string
		jtype                                   string
	)
	if err := row.Scan(&jname, &jtype, &params, &args, &spec, &enabled, &created, &updated,
		&lastRun, &lastOK, &lastMS, &lastError, &lastOutput); err != nil {
		return Job{}, err
	}
	job.Name, job.Type, job.Spec = jname, jtype, spec
	job.Enabled = enabled != 0
	job.LastOK = lastOK != 0
	job.LastDuration = time.Duration(lastMS) * time.Millisecond
	job.LastError, job.LastOutput = lastError, lastOutput
	// Unreadable parameters are a corrupted row rather than a job with none: a
	// ticket digest with no query is not a smaller version of the job, it is a
	// different one, and the caller is better told than handed it.
	if err := decodeJobJSON(params, &job.Params); err != nil {
		return Job{}, fmt.Errorf("the stored parameters of job %s are unreadable: %w", jname, err)
	}
	// The legacy column, which only the migration reads. It is tolerated rather
	// than enforced: an untyped row IS the thing being migrated, and refusing to
	// load it would leave the admin with a job they can neither run nor see.
	if err := decodeJobJSON(args, &job.LegacyArgs); err != nil {
		return Job{}, fmt.Errorf("the stored arguments of job %s are unreadable: %w", jname, err)
	}
	job.CreatedAt = parseJobTime(created)
	job.UpdatedAt = parseJobTime(updated)
	job.LastRun = parseJobTime(lastRun)
	return job, nil
}

// decodeJobJSON reads one of the two JSON columns into dest.
//
// An empty string decodes to the zero value rather than to an error. Both
// columns are DEFAULT ” on a ledger that predates them, and "this row was
// written before the column existed" is a fact to carry forward, not a
// corruption to refuse.
func decodeJobJSON(raw string, dest any) error {
	if raw == "" {
		return nil
	}
	return json.Unmarshal([]byte(raw), dest)
}

// parseJobTime reads a stored timestamp, treating an empty or unparseable one
// as the zero time — which for last_run_at means "has never run".
func parseJobTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.Local()
}
