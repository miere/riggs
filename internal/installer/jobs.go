package installer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/schedule"
)

// Seeding Riggs' own schedule.
//
// This replaces the step that registered jobs with Murtaugh through its CLI.
// Riggs now runs its own (§9c), so the install writes them into the ledger it
// is about to hand the daemon — one fewer tool in the chain, and one fewer
// place for the two of them to disagree about what is running.
//
// It is deliberately not silent about the handover. Two schedulers driving one
// digest is noise rather than redundancy: every pull request announced twice,
// by two processes racing each other to write the same ledger rows. Nothing
// here can remove Murtaugh's copies — that is its config, not ours — so the
// install prints the commands and says why.

// gatherJobs asks about, builds and stores the schedule.
func (i *Installer) gatherJobs(ctx context.Context, configPath string) error {
	i.p.Say("")
	i.p.Say("Riggs runs its own schedule now, inside the daemon. These are the two")
	i.p.Say("jobs it takes over from Murtaugh; both can be edited later from the")
	i.p.Say("App Home tab, or with `riggs jobs`.")

	install, err := i.p.Confirm("Set them up?", true)
	if err != nil {
		return err
	}
	if !install {
		i.p.Say("Skipped. `riggs jobs import` adopts them later.")
		return nil
	}

	jobs, skipped, err := schedule.Adopted(i.ghLogin, "")
	if err != nil {
		return err
	}
	for i := range jobs {
		// Nothing is asked about the cadence. A migration that also changes the
		// schedule makes it impossible to attribute a behaviour difference.
		jobs[i].UpdatedAt = time.Now()
	}
	jobs, err = i.askDelivery(jobs)
	if err != nil {
		return err
	}

	if err := i.saveJobs(config.DBPathFor(configPath), jobs); err != nil {
		return fmt.Errorf("storing the schedule: %w", err)
	}

	i.p.Say("")
	for _, job := range jobs {
		i.p.Say("  scheduled %s (%s): %s", job.Name, job.Spec, schedule.Command(job))
	}
	for _, note := range skipped {
		i.p.Say("  skipped   %s", note)
	}
	if len(jobs) > 0 {
		i.p.Say("")
		i.p.Say("Riggs owns these now. If Murtaugh still has copies, remove them or both will run:")
		for _, name := range schedule.AdoptedNames {
			i.p.Say("  murtaugh jobs remove --name %s", name)
		}
	}
	return nil
}

// askDelivery collects where each digest is posted and which app posts it.
//
// The profile is not cosmetic and is the reason this is asked at all: a click is
// delivered to the app that POSTED the message, so a digest sent through the
// wrong profile renders a menu the daemon never hears about.
func (i *Installer) askDelivery(jobs []notify.Job) ([]notify.Job, error) {
	out := make([]notify.Job, 0, len(jobs))
	for _, job := range jobs {
		i.p.Say("")
		i.p.Say("  %s — %s", job.Name, schedule.Command(job))
		channel, err := i.p.Ask("    Channel (empty = DM you)", "")
		if err != nil {
			return nil, err
		}
		profile, err := i.p.Ask("    Slack profile to post as (must match the daemon's)", config.DefaultProfile)
		if err != nil {
			return nil, err
		}
		if c := strings.TrimSpace(channel); c != "" {
			job.Args = append(job.Args, "--slack-channel", c)
		}
		if p := strings.TrimSpace(profile); p != "" {
			job.Args = append(job.Args, "--slack-profile", p)
		}
		out = append(out, job)
	}
	return out, nil
}

// storeJobs writes the seeded schedule to the ledger.
//
// It opens the ledger and closes it again rather than holding one: the
// installer is a one-shot, and the daemon it is provisioning will open its own.
func storeJobs(dbPath string, jobs []notify.Job) error {
	store, err := notify.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	for _, job := range jobs {
		// An existing job is left alone. Re-running the installer must not undo
		// a schedule somebody has since edited.
		if _, exists, err := store.Job(ctx, job.Name); err != nil {
			return err
		} else if exists {
			continue
		}
		if err := store.SaveJob(ctx, job); err != nil {
			return err
		}
	}
	return nil
}
