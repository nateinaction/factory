// Package boundedfile opens a file foreman did not write, such as a model's output file or a
// repository's documents, without trusting what is there: only a regular file is opened, and the
// caller reads it through a limit rather than whole.
package boundedfile

import (
	"errors"
	"fmt"
	"os"
)

// Open opens name under root for reading. root keeps the read inside its directory: a name that
// resolves outside it fails rather than opening. Only a regular file is opened: a symlink, a device,
// a FIFO or a directory is not a file foreman reads.
func Open(root *os.Root, name string) (*os.File, error) {
	// The name is stat'ed before it is opened: opening a FIFO read-only blocks until a writer
	// appears, which for a file the model chose would stall the run with no error at all.
	info, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", name, err)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}

	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}

	// What is read is the open file, so the open file is what must be regular: the name could have
	// been swapped between the stat and the open.
	opened, err := f.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("stat %s: %w", name, err), f.Close())
	}

	if !opened.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("%s is not a regular file", name), f.Close())
	}

	return f, nil
}
