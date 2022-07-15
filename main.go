package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	nomad "github.com/hashicorp/nomad/api"
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

type jobTask struct {
	job  string
	task string
}

func run(args []string) error {
	var (
		rootFlagSet = flag.NewFlagSet("nomadlogs", flag.ExitOnError)
		addr        = rootFlagSet.String("addr", nomad.DefaultConfig().Address, "nomad address")

		watchFlagSet = flag.NewFlagSet("nomadlogs watch", flag.ExitOnError)
		jobsToTasks  = watchFlagSet.String("jobs", "", "comma-separated list of job:task to watch")
	)

	list := &ffcli.Command{
		Name:       "list",
		ShortUsage: "nomadlogs [flags] list",
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
		ShortUsage: "nomadlogs [flags] watch -jobs <job-a>:<allocation-a>",
		ShortHelp:  "watch jobs:allocations",
		FlagSet:    watchFlagSet,
		Exec: func(_ context.Context, _ []string) error {
			cfg := nomad.DefaultConfig()
			cfg.Address = *addr

			client, err := nomad.NewClient(cfg)
			if err != nil {
				return fmt.Errorf("could not create nomad client: %s", err)
			}

			var jobTasks []jobTask
			for _, jobToTask := range strings.Split(*jobsToTasks, ",") {
				s := strings.Split(jobToTask, ":")
				if len(s) != 2 {
					return fmt.Errorf("jobs must be specified in the format job:task")
				}

				job, task := s[0], s[1]
				jobTasks = append(jobTasks, jobTask{
					job:  job,
					task: task,
				})
			}

			w := watcher{
				jobTasks: jobTasks,
				client:   client,

				allocationsWatched: make(map[string]struct{}),
			}

			return w.run()
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

type watcher struct {
	jobTasks []jobTask
	client   *nomad.Client

	mu                 sync.Mutex
	allocationsWatched map[string]struct{}
}

const waitDuration = 5 * time.Second

func (w *watcher) run() error {
	for _, jobTask := range w.jobTasks {
		log.Printf("watching job %s, task %s", jobTask.job, jobTask.task)
	}

	for range time.Tick(waitDuration) {
		allocationList, _, err := w.client.Allocations().List(nil)
		if err != nil {
			log.Printf("could not list nomad allocations. waiting %s before trying again: %s", waitDuration, err)
			continue
		}

		allocationsToWatch := []string{}
		for _, allocationStub := range allocationList {
			for _, jobTask := range w.jobTasks {
				if allocationStub.JobID == jobTask.job && allocationStub.ClientStatus == "running" {
					allocationsToWatch = append(allocationsToWatch, allocationStub.ID)
				}
			}
		}

		for _, allocationID := range allocationsToWatch {
			if _, alreadyWatching := w.allocationsWatched[allocationID]; alreadyWatching {
				continue
			}

			allocation, _, err := w.client.Allocations().Info(allocationID, nil)
			if err != nil {
				// The allocation probably went away before we could query it
				// specifically.
				log.Printf("could not retrieve allocation %s", allocationID)
				continue
			}

			go func(allocationID string) {
				w.mu.Lock()
				w.allocationsWatched[allocationID] = struct{}{}
				w.mu.Unlock()

				log.Printf("watching allocation %+v", allocation)

				aw := allocationWatcher{
					allocation: allocation,
					client:     w.client,
				}

				// watch the allocation until it's done
				aw.watch()

				w.mu.Lock()
				delete(w.allocationsWatched, allocationID)
				w.mu.Unlock()
			}(allocationID)
		}
	}

	return nil
}

type allocationWatcher struct {
	allocation *nomad.Allocation
	client     *nomad.Client
	logPrefix  string
}

func newAllocationWatcher(allocation *nomad.Allocation, client *nomad.Client) allocationWatcher {
	return allocationWatcher{
		allocation: allocation,
		client:     client,
		logPrefix:  fmt.Sprintf("%s(%s)", allocation.JobID, allocation.ID[:6]),
	}
}

func (aw *allocationWatcher) watch() {
	stdoutFrames, stdoutErrChan := aw.client.AllocFS().Logs(aw.allocation, true, aw.allocation.TaskGroup, "stdout", "end", 0, nil, nil)
	stderrFrames, stderrErrChan := aw.client.AllocFS().Logs(aw.allocation, true, aw.allocation.TaskGroup, "stderr", "end", 0, nil, nil)

	for {
		select {
		case stdoutFrame, more := <-stdoutFrames:
			if !more {
				log.Printf("stdoutFrames closed!")
				return
			}
			for _, line := range strings.Split(string(stdoutFrame.Data), "\n") {
				if line == "" {
					continue
				}
				fmt.Printf("%s: %s\n", aw.logPrefix, line)
			}
		case stderrFrame, more := <-stderrFrames:
			if !more {
				log.Printf("stderrFrames closed!")
				return
			}
			for _, line := range strings.Split(string(stderrFrame.Data), "\n") {
				if line == "" {
					continue
				}
				fmt.Printf("%s: %s\n", aw.logPrefix, line)
			}
		case err := <-stdoutErrChan:
			log.Printf("%s: got error (allocation probably shutting down): %s", aw.logPrefix, err)
			return
		case err := <-stderrErrChan:
			log.Printf("%s: got error (allocation probably shutting down): %s", aw.logPrefix, err)
			return
		}
	}
}
