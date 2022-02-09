package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	nomad "github.com/hashicorp/nomad/api"
	okrun "github.com/oklog/run"
	"github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
)

func main() {
	// write meta logs to stderr, actual program output to stdout
	log.SetOutput(os.Stderr)

	log.SetPrefix("nomadlogs ")
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	watchFlagSet := flag.NewFlagSet("nomadlogs", flag.ExitOnError)
	var (
		jobs = watchFlagSet.String("jobs", "", "comma-separated list of job:task to watch")
		addr = watchFlagSet.String("addr", "http://127.0.0.1:4646", "nomad address")
	)

	if err := ff.Parse(watchFlagSet, args, ff.WithEnvVarPrefix("NOMAD_LOGS")); err != nil {
		return fmt.Errorf("could not parse flags: %s", err)
	}

	watch := &ffcli.Command{
		Name:       "watch",
		ShortUsage: "nomadlogs watch [<arg> ...]",
		ShortHelp:  "watch the number of bytes in the arguments.",
		FlagSet:    watchFlagSet,
		Exec: func(ctx context.Context, args []string) error {
			cfg := nomad.DefaultConfig()
			cfg.Address = *addr

			client, err := nomad.NewClient(cfg)
			if err != nil {
				return fmt.Errorf("could not create nomad client: %s", err)
			}

			var g okrun.Group
			for _, jobToTask := range strings.Split(*jobs, ",") {
				jobToTask := jobToTask
				g.Add(func() error {
					s := strings.Split(jobToTask, ":")
					if len(s) != 2 {
						return fmt.Errorf("jobs must be specified in the format job:task")
					}

					job, task := s[0], s[1]

					jw := jobWatcher{
						job:    job,
						task:   task,
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
		},
	}

	root := &ffcli.Command{
		ShortUsage:  "nomadlogs [flags] <subcommand>",
		Subcommands: []*ffcli.Command{watch},
	}

	if err := root.ParseAndRun(context.Background(), os.Args[1:]); err != nil {
		return err
	}

	return nil
}

type jobWatcher struct {
	job    string
	task   string
	client *nomad.Client
}

const waitDuration = 5 * time.Second

func (jw *jobWatcher) run(ctx context.Context) error {
	log.Printf("watching job %s, task %s\n", jw.job, jw.task)

	for {
		allocationList, _, err := jw.client.Allocations().List(nil)
		if err != nil {
			log.Printf("could not list nomad allocations. nomad probably isn't running? waiting %s before trying again: %s\n", waitDuration, err)
			time.Sleep(waitDuration)
			continue
		}

		var allocationID string
		for _, allocationStub := range allocationList {
			if allocationStub.JobID == jw.job && allocationStub.ClientStatus == "running" {
				allocationID = allocationStub.ID
				break
			}
		}

		if allocationID == "" {
			log.Printf("no allocations running for %s; waiting for %s before trying again\n", jw.job, waitDuration)
			time.Sleep(waitDuration)
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
	stdoutFrames, stdoutErrChan := jw.client.AllocFS().Logs(allocation, true, jw.task, "stdout", "end", 0, nil, nil)
	stderrFrames, stderrErrChan := jw.client.AllocFS().Logs(allocation, true, jw.task, "stderr", "end", 0, nil, nil)

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

				fmt.Printf("%s: %s\n", jw.job, s)
			}

		case stderrFrame := <-stderrFrames:
			// We're probably getting multiple lines.
			raw := string(stderrFrame.Data)
			ss := strings.Split(raw, "\n")

			for _, s := range ss {
				if s == "" {
					continue
				}

				fmt.Printf("%s: %s\n", jw.job, s)
			}

		case err := <-stdoutErrChan:
			log.Printf("%s: got error (allocation probably shutting down): %s\n", jw.job, err)
			return nil
		case err := <-stderrErrChan:
			log.Printf("%s: got error (allocation probably shutting down): %s\n", jw.job, err)
			return nil
		}
	}

}
