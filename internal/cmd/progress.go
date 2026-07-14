package cmd

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mlahr/snapzner/internal/snapzner"
	"golang.org/x/term"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type backupProgressRenderer struct {
	out      io.Writer
	quiet    bool
	dynamic  bool
	mu       sync.Mutex
	active   map[string]snapzner.Progress
	current  string
	frame    int
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func newBackupProgressRenderer(out io.Writer, quiet bool) *backupProgressRenderer {
	renderer := &backupProgressRenderer{
		out: out, quiet: quiet, dynamic: writerIsTerminal(out),
		active: map[string]snapzner.Progress{}, stop: make(chan struct{}), done: make(chan struct{}),
	}
	if quiet || !renderer.dynamic {
		close(renderer.done)
		return renderer
	}
	go renderer.animate()
	return renderer
}

func (r *backupProgressRenderer) Report(progress snapzner.Progress) {
	if r.quiet {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	key := progressKey(progress)
	if !r.dynamic {
		fmt.Fprintln(r.out, formatProgress(progress))
		return
	}

	r.clearLocked()
	switch progress.Message {
	case "creating snapshot":
		r.active[key] = progress
		r.current = key
		r.renderSpinnerLocked()
	case "snapshot available", "snapshot failed":
		delete(r.active, key)
		fmt.Fprintln(r.out, formatProgress(progress))
		r.selectCurrentLocked()
		r.renderSpinnerLocked()
	default:
		fmt.Fprintln(r.out, formatProgress(progress))
		r.renderSpinnerLocked()
	}
}

func (r *backupProgressRenderer) Close() {
	if r.dynamic && !r.quiet {
		r.stopOnce.Do(func() { close(r.stop) })
		<-r.done
		r.mu.Lock()
		r.clearLocked()
		r.mu.Unlock()
	}
}

func (r *backupProgressRenderer) animate() {
	ticker := time.NewTicker(80 * time.Millisecond)
	defer func() {
		ticker.Stop()
		close(r.done)
	}()
	for {
		select {
		case <-ticker.C:
			r.mu.Lock()
			if len(r.active) > 0 {
				r.frame = (r.frame + 1) % len(spinnerFrames)
				r.renderSpinnerLocked()
			}
			r.mu.Unlock()
		case <-r.stop:
			return
		}
	}
}

func (r *backupProgressRenderer) renderSpinnerLocked() {
	progress, ok := r.active[r.current]
	if !ok {
		return
	}
	r.clearLocked()
	line := formatProgress(progress)
	if len(r.active) > 1 {
		line += fmt.Sprintf(" (%d active)", len(r.active))
	}
	fmt.Fprintf(r.out, "%s %s", spinnerFrames[r.frame], line)
}

func (r *backupProgressRenderer) clearLocked() {
	if r.dynamic {
		fmt.Fprint(r.out, "\r\x1b[2K")
	}
}

func (r *backupProgressRenderer) selectCurrentLocked() {
	if _, ok := r.active[r.current]; ok {
		return
	}
	r.current = ""
	for key := range r.active {
		r.current = key
		break
	}
}

func progressKey(progress snapzner.Progress) string {
	return fmt.Sprintf("%s/%d", progress.Project, progress.ServerID)
}

func formatProgress(progress snapzner.Progress) string {
	prefix := fmt.Sprintf("[%s]", progress.Project)
	if progress.ServerID != 0 {
		prefix += fmt.Sprintf(" %s (server %d)", progress.ServerName, progress.ServerID)
	}
	if progress.Completed > 0 && progress.Total > 0 {
		prefix += fmt.Sprintf(" [%d/%d]", progress.Completed, progress.Total)
	}
	return prefix + " " + progress.Message
}

func writerIsTerminal(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}
