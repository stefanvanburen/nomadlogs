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
	"github.com/peterbourgon/ff/v3/ffcli"
)

func main() {
	// write meta logs to stderr, actual program output to stdout
	log.SetOutput(os.Stderr)
	log.SetPrefix("nomadlogs ")

	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var (
		rootFlagSet = flag.NewFlagSet("nomadlogs", flag.ExitOnError)
		addr        = rootFlagSet.String("addr", nomad.DefaultConfig().Address, "nomad address")

		watchFlagSet = flag.NewFlagSet("nomadlogs watch", flag.ExitOnError)
		jobs         = watchFlagSet.String("jobs", "", "comma-separated list of job:task to watch")
	)

	list := &ffcli.Command{
		Name:       "list",
		ShortUsage: "nomadlogs list",
		ShortHelp:  "list jobs:allocations",
		Exec: func(_ context.Context, _ []string) error {
			cfg := nomad.DefaultConfig()
			cfg.Address = *addr
			client, err := nomad.NewClient(cfg)
			if err != nil {
				return fmt.Errorf("could not create nomad client: %s", err)
			}
			list, _, err := client.Allocations().List(nil)
			if err != nil {
				return fmt.Errorf("could not get allocations: %s", err)
			}
			for _, allocation := range list {
				for task := range allocation.TaskStates {
					fmt.Printf("%s:%s\n", allocation.JobID, task)
				}
			}
			return nil
		},
	}

	watch := &ffcli.Command{
		Name:       "watch",
		ShortUsage: "nomadlogs watch",
		ShortHelp:  "watch jobs:allocations",
		FlagSet:    watchFlagSet,
		Exec: func(_ context.Context, _ []string) error {
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

					return jw.run()
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
		FlagSet:     rootFlagSet,
		Subcommands: []*ffcli.Command{watch, list},
	}

	if err := root.ParseAndRun(context.Background(), args); err != nil {
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

func (jw *jobWatcher) run() error {
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
		jw.watchStream(allocation)
	}
}

func (jw *jobWatcher) watchStream(allocation *nomad.Allocation) error {
	stdoutFrames, stdoutErrChan := jw.client.AllocFS().Logs(allocation, true, jw.task, "stdout", "end", 0, nil, nil)
	stderrFrames, stderrErrChan := jw.client.AllocFS().Logs(allocation, true, jw.task, "stderr", "end", 0, nil, nil)

	for {
		select {
		case stdoutFrame, more := <-stdoutFrames:
			if !more {
				log.Printf("stdoutFrames closed!")
				return nil
			}
			if stdoutFrame == nil {
				log.Printf("got nil stdout frame, skipping")
				continue
			}

			// We're probably getting multiple lines.
			raw := string(stdoutFrame.Data)
			ss := strings.Split(raw, "\n")

			for _, s := range ss {
				if s == "" {
					continue
				}

				fmt.Printf("%s(%s): %s\n", jw.job, allocation.ID[:6], s)
			}

		case stderrFrame, more := <-stderrFrames:
			if !more {
				log.Printf("stderrFrames closed!")
				return nil
			}
			if stderrFrame == nil {
				log.Printf("got nil stderr frame")
				continue
			}

			// We're probably getting multiple lines.
			raw := string(stderrFrame.Data)
			ss := strings.Split(raw, "\n")

			for _, s := range ss {
				if s == "" {
					continue
				}

				fmt.Printf("%s(%s): %s\n", jw.job, allocation.ID[:6], s)
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
