package main

import (
	"fmt"
	"os"

	"github.com/wojciech-malota-wojcik/isolator"
	"github.com/wojciech-malota-wojcik/isolator/client/wire"
)

func main() {
	config := isolator.Config{
		// Directory where container is created, filesystem of container should exist inside "root" directory there
		Dir: "/tmp/example",
		Executor: wire.Config{
			Mounts: []wire.Mount{
				// Let's make host's /tmp available inside container under /test
				{
					Host:      "/tmp",
					Container: "/test",
				},
			},
		},
	}

	// Starting isolator. If passed ctx is canceled, isolator.Start breaks and returns error.
	// Isolator creates `root` directory under one passed to `isolator.Start`. The `root` directory is mounted as `/`.
	// inside container.
	// It is assumed that `root` contains `bin/sh` shell and all the required libraries. Without them it will fail.
	client, terminateIsolator, err := isolator.Start(config)
	if err != nil {
		panic(err)
	}
	defer func() {
		// Clean up on exit
		if err := terminateIsolator(); err != nil {
			panic(err)
		}
	}()

	// Request to execute command in isolation
	if err := client.Send(wire.Execute{Command: `echo "Hello world!"`}); err != nil {
		panic(err)
	}

	// Communication channel loop
	for {
		msg, err := client.Receive()
		if err != nil {
			panic(err)
		}
		switch m := msg.(type) {
		// wire.Log contains message printed by executed command to stdout or stderr
		case wire.Log:
			stream, err := toStream(m.Stream)
			if err != nil {
				panic(err)
			}
			if _, err := stream.Write([]byte(m.Text)); err != nil {
				panic(err)
			}
		// wire.Completed means command finished
		case wire.Completed:
			if m.ExitCode != 0 || m.Error != "" {
				panic(fmt.Errorf("command failed: %s, exit code: %d", m.Error, m.ExitCode))
			}
			return
		default:
			panic("unexpected message received")
		}
	}
}

func toStream(stream wire.Stream) (*os.File, error) {
	var f *os.File
	switch stream {
	case wire.StreamOut:
		f = os.Stdout
	case wire.StreamErr:
		f = os.Stderr
	default:
		return nil, fmt.Errorf("unknown stream: %d", stream)
	}
	return f, nil
}
