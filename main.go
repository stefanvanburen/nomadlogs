package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	okrun "github.com/oklog/run"
	"github.com/peterbourgon/ff/v3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Printf("nomadlogs err: %s", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("nomadlogs", flag.ExitOnError)
	var (
		// noteadmin,notesim-api
		// job                  -> task
		// public_notediscovery -> notediscovery
		// noteboard            -> noteboard
		// public_notehandler   -> public_notehandler
		// ...
		jobs = fs.String("jobs", "noteboard,public_notehandler", "comma-separated list of jobs to watch")
	)

	if err := ff.Parse(fs, args, ff.WithEnvVarPrefix("NOMAD_LOGS")); err != nil {
		return fmt.Errorf("could not parse flags: %s", err)
	}

	client, err := nomad.NewClient(nomad.DefaultConfig())
	if err != nil {
		return fmt.Errorf("could not create nomad client: %s", err)
	}

	ctx := context.Background()

	var g okrun.Group
	for _, job := range strings.Split(*jobs, ",") {
		job := job
		g.Add(func() error {
			jw := jobWatcher{
				name:   job,
				client: client,
			}

			return jw.run(ctx)
		}, func(err error) {

		})
	}

	if err := g.Run(); err != nil {
		return fmt.Errorf("got error: %s", err)
	}

	return nil
}

type jobWatcher struct {
	name   string
	client *nomad.Client
}

func (jw *jobWatcher) run(ctx context.Context) error {
	fmt.Printf("watching job %s\n", jw.name)

	for {
		allocationList, _, err := jw.client.Allocations().List(nil)
		if err != nil {
			fmt.Printf("meta: could not list nomad allocations. nomad probably isn't running? waiting 30 seconds before trying again: %s\n", err)
			time.Sleep(30 * time.Second)
			continue
		}

		var allocationID string
		for _, allocationStub := range allocationList {
			if allocationStub.JobID == jw.name && allocationStub.ClientStatus == "running" {
				allocationID = allocationStub.ID
				break
			}
		}

		if allocationID == "" {
			fmt.Printf("meta: no allocations running for %s; waiting for 30 seconds before trying again\n", jw.name)
			time.Sleep(30 * time.Second)
			continue
		}

		allocation, _, err := jw.client.Allocations().Info(allocationID, nil)
		if err != nil {
			return fmt.Errorf("could not retrieve allocation: %s", err)
		}

		// watch the stream until it's done
		jw.watchStream(ctx, allocation)
	}
}

func (jw *jobWatcher) watchStream(ctx context.Context, allocation *nomad.Allocation) error {
	stdoutFrames, stdoutErrChan := jw.client.AllocFS().Logs(allocation, true, jw.name, "stdout", "end", 0, nil, nil)
	stderrFrames, stderrErrChan := jw.client.AllocFS().Logs(allocation, true, jw.name, "stderr", "end", 0, nil, nil)

	for {
		select {
		case stdoutFrame := <-stdoutFrames:
			// We're probably getting multiple lines.
			raw := string(stdoutFrame.Data)
			ss := strings.Split(raw, "\n")

			for _, s := range ss {
				if s == "" {
					continue
				}

				fmt.Printf("%s: %s\n", jw.name, s)
			}

		case stderrFrame := <-stderrFrames:
			// We're probably getting multiple lines.
			raw := string(stderrFrame.Data)
			ss := strings.Split(raw, "\n")

			for _, s := range ss {
				if s == "" {
					continue
				}

				fmt.Printf("%s: %s\n", jw.name, s)
			}

		case err := <-stdoutErrChan:
			fmt.Printf("meta: %s: got error (allocation probably shutting down): %s\n", jw.name, err)
			return nil
		case err := <-stderrErrChan:
			fmt.Printf("meta: %s: got error (allocation probably shutting down): %s\n", jw.name, err)
			return nil
		}
	}

}
