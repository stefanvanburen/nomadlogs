# nomadlogs

CLI for combining logs from multiple [Nomad](https://github.com/hashicorp/nomad) jobs / tasks / allocations into a single stream.

## Installation

If you have `go` installed, you can install the binary with:

```sh
go install github.com/stefanvanburen/nomadlogs@latest
```

## Running

```sh
nomadlogs watch -jobs job1:task1,job2:task1,job2:task2
```

By default, nomadlogs will use the Nomad SDK's default configuration for Nomad at `http://127.0.0.1:4646`.

Also, if you're forwarding another instance of Nomad to another port, say `:4647`, you can run:

```sh
nomadlogs -addr "http://localhost:4647" watch -jobs job1:task1,job2:task1,job2:task2
```
