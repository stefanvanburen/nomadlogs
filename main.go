package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	okrun "github.com/oklog/run"
	"github.com/peterbourgon/ff/v3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Printf("vagrant-logs err: %s", err)
		os.Exit(1)
	}
}

// Still TODO:
// 1. when the watchers go down, try to start up again at some point in the future?
//    Maybe a ticker that wraps the jobs?
// 2. confirm that when we ctrl-c out, we successfully shut down the goroutines

func run(args []string) error {
	fs := flag.NewFlagSet("vagrant-logs", flag.ExitOnError)
	var (
		jobs = fs.String("services", "noteboard,noteadmin,notesim-api,public_notehandler,public_notediscovery", "comma-separated list of jobs to watch")
	)

	if err := ff.Parse(fs, args, ff.WithEnvVarPrefix("VAGRANT_LOGS")); err != nil {
		return fmt.Errorf("could not parse flags: %s", err)
	}

	jobsToWatch := strings.Split(*jobs, ",")

	logs := make(chan msg, 100)

	ctx := context.Background()
	var g okrun.Group

	for _, job := range jobsToWatch {
		job := job
		// watch stdout
		g.Add(func() error {
			return watchJob(ctx, job, logs, false)
		}, func(err error) {
			// TODO: anything useful to do here?
		})

		// watch stderr
		g.Add(func() error {
			return watchJob(ctx, job, logs, true)
		}, func(err error) {
			// TODO: anything useful to do here?
		})
	}

	g.Add(func() error {
		return printLogs(ctx, logs)
	}, func(err error) {
		// TODO: anything useful to do here?
	})

	if err := g.Run(); err != nil {
		return fmt.Errorf("error watching job: %s", err)
	}

	return nil
}

type msg struct {
	service string
	log     string
}

func watchJob(ctx context.Context, job string, logs chan<- msg, isStdErr bool) error {
	commandName := "vagrant"
	args := []string{"ssh", "client", "--", "nomad", "alloc", "logs", "-tail", "-f", "-job", job}
	if isStdErr {
		args = []string{"ssh", "client", "--", "nomad", "alloc", "logs", "-tail", "-f", "-stderr", "-job", job}
	}

	cmd := exec.CommandContext(ctx, commandName, args...)

	cmdOut, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("could not get pipe to stdout: %s", err)
	}

	fmt.Printf("starting command %s...\n", cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start command: %s", err)
	}

	scanner := bufio.NewScanner(cmdOut)

	for scanner.Scan() {
		logs <- msg{
			service: job,
			log:     scanner.Text(),
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %s", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("error waiting for command: %s", err)
	}

	return nil
}

func printLogs(ctx context.Context, logs <-chan msg) error {
	for {
		select {
		case log := <-logs:
			fmt.Printf("%s: %s\n", log.service, log.log)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
