package modulir

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Public
//
//
//
//////////////////////////////////////////////////////////////////////////////

// Config contains configuration.
type Config struct {
	// Concurrency is the number of concurrent workers to run during the build
	// step.
	//
	// Defaults to 10.
	Concurrency int

	// Log specifies a logger to use.
	//
	// Defaults to an instance of Logger running at informational level.
	Log LoggerInterface

	// Port specifies the port on which to serve content from TargetDir over
	// HTTP.
	//
	// Defaults to not running if left unset.
	Port int

	// SourceDir is the directory containing source files.
	//
	// Defaults to ".".
	SourceDir string

	// TargetDir is the directory where the site will be built to.
	//
	// Defaults to "./public".
	TargetDir string

	// Websocket indicates that Modulir should be started in development
	// mode with a websocket that provides features like live reload.
	//
	// Defaults to false.
	Websocket bool
}

// Build is one of the main entry points to the program. Call this to build
// only one time.
func Build(config *Config, f func(*Context) []error) {
	var buildCompleteMu sync.Mutex
	buildComplete := sync.NewCond(&buildCompleteMu)
	finish := make(chan struct{}, 1)

	// Signal the build loop to finish immediately
	finish <- struct{}{}

	c := initContext(config, nil)
	success := build(c, f, finish, buildComplete)
	if !success {
		os.Exit(1)
	}
}

// BuildLoop is one of the main entry points to the program. Call this to build
// in a perpetual loop.
func BuildLoop(config *Config, f func(*Context) []error) {
	var buildCompleteMu sync.Mutex
	buildComplete := sync.NewCond(&buildCompleteMu)
	finish := make(chan struct{}, 1)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		exitWithError(errors.Wrap(err, "Error starting watcher"))
		os.Exit(1)
	}
	defer watcher.Close()

	c := initContext(config, watcher)

	// Serve HTTP
	var server *http.Server
	go func() {
		// Wait for the first build to complete before serving anything.
		//
		// Note: this is probably only the right move if the target directory
		// doesn't exist yet. Otherwise it doesn't really matter. TODO: Look
		// into that more.
		buildComplete.L.Lock()
		buildComplete.Wait()
		buildComplete.L.Unlock()

		server = startServingTargetDirHTTP(c, buildComplete)
	}()

	// Run the build loop. Loops forever until receiving on finish.
	go build(c, f, finish, buildComplete)

	// Listen for signals
	signals := make(chan os.Signal, 1024)
	signal.Notify(signals, unix.SIGUSR2)
	for {
		s := <-signals
		switch s {
		case unix.SIGUSR2:
			shutdownAndExec(c, finish, watcher, server)
		}
	}
}

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Private
//
//
//
//////////////////////////////////////////////////////////////////////////////

const (
	// Maximum number of errors or jobs to print on screen after a build loop.
	maxMessages = 10
)

// Runs an infinite built loop until a signal is received over the `finish`
// channel.
//
// Returns true of the last build was successful and false otherwise.
func build(c *Context, f func(*Context) []error,
	finish chan struct{}, buildComplete *sync.Cond) bool {

	rebuild := make(chan map[string]struct{})
	rebuildDone := make(chan struct{})

	if c.Watcher != nil {
		go watchChanges(c, c.Watcher, rebuild, rebuildDone)
	}

	c.Pool.StartRound()
	c.Jobs = c.Pool.Jobs

	// Paths that changed on the last loop (as discovered via fsnotify). If
	// set, we go into quick build mode with only these paths activated, and
	// unset them afterwards. This saves us doing lots of checks on the
	// filesystem and makes jobs much faster to run.
	var lastChangedSources map[string]struct{}

	for {
		c.Log.Debugf("Start loop")
		c.ResetBuild()

		if lastChangedSources != nil {
			c.QuickPaths = lastChangedSources
		}

		errors := f(c)

		lastRoundErrors := c.Wait()
		buildDuration := time.Now().Sub(c.Stats.Start)

		if lastRoundErrors != nil {
			errors = append(errors, lastRoundErrors...)
		}

		logErrors(c, errors)
		logSlowestJobs(c)

		c.Log.Infof("Built site in %s (%v / %v job(s) did work; %v errored; loop took %v)",
			buildDuration,
			c.Stats.NumJobsExecuted, c.Stats.NumJobs, c.Stats.NumJobsErrored,
			c.Stats.LoopDuration)

		lastChangedSources = nil
		c.QuickPaths = nil

		buildComplete.Broadcast()

		if c.FirstRun {
			c.FirstRun = false
		} else {
			rebuildDone <- struct{}{}
		}

		select {
		case <-finish:
			c.Log.Infof("Detected finish signal; stopping")
			return len(errors) < 1

		case lastChangedSources = <-rebuild:
			c.Log.Infof("Detected change on %v; rebuilding", mapKeys(lastChangedSources))
		}
	}
}

// Exits with status 1 after printing the given error to stderr.
func exitWithError(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

// Takes a Modulir configuration and initializes it with defaults for any
// properties that weren't expressly filled in.
func initConfigDefaults(config *Config) *Config {
	if config == nil {
		config = &Config{}
	}

	if config.Concurrency <= 0 {
		config.Concurrency = 50
	}

	if config.Log == nil {
		config.Log = &Logger{Level: LevelInfo}
	}

	if config.SourceDir == "" {
		config.SourceDir = "."
	}

	if config.TargetDir == "" {
		config.TargetDir = "./public"
	}

	return config
}

// Initializes a new Modulir context from the given configuration.
func initContext(config *Config, watcher *fsnotify.Watcher) *Context {
	config = initConfigDefaults(config)

	return NewContext(&Args{
		Log:       config.Log,
		Port:      config.Port,
		Pool:      NewPool(config.Log, config.Concurrency),
		SourceDir: config.SourceDir,
		TargetDir: config.TargetDir,
		Watcher:   watcher,
		Websocket: config.Websocket,
	})
}

// Log a limited set of errors that occurred during a build.
func logErrors(c *Context, errors []error) {
	if errors == nil {
		return
	}

	for i, err := range errors {
		// When dealing with an errored job (in practice, this is going to be
		// the common case), we can provide a little more detail on what went
		// wrong.
		job, ok := err.(*Job)

		if ok {
			c.Log.Errorf("Job error: %v (job: '%s', time: %v)",
				job.Err, job.Name, job.Duration)
		} else {
			c.Log.Errorf("Build error: %v", err)
		}

		if i >= maxMessages-1 {
			c.Log.Errorf("... too many errors (limit reached)")
			break
		}
	}
}

// Log a limited set of executed jobs from the last build.
func logSlowestJobs(c *Context) {
	sortJobsBySlowest(c.Stats.JobsExecuted)

	for i, job := range c.Stats.JobsExecuted {
		// Having this in the loop ensures we don't print it if zero jobs
		// executed
		if i == 0 {
			c.Log.Infof("Jobs executed (slowest first):")
		}

		c.Log.Infof("    %s (time: %v)", job.Name, job.Duration)

		if i >= maxMessages-1 {
			c.Log.Infof("... many jobs executed (limit reached)")
			break
		}
	}
}

// Extract the names of keys out of a map and return them as a slice.
func mapKeys(m map[string]struct{}) []string {
	var keys []string
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// Decides whether a rebuild should be triggered given some input event
// properties from fsnotify.
func shouldRebuild(path string, op fsnotify.Op) bool {
	base := filepath.Base(path)

	// Mac OS' worst mistake.
	if base == ".DS_Store" {
		return false
	}

	// Vim creates this temporary file to see whether it can write into a
	// target directory. It screws up our watching algorithm, so ignore it.
	if base == "4913" {
		return false
	}

	// A special case, but ignore creates on files that look like Vim backups.
	if strings.HasSuffix(base, "~") {
		return false
	}

	if op&fsnotify.Create != 0 {
		return true
	}

	if op&fsnotify.Remove != 0 {
		return true
	}

	if op&fsnotify.Write != 0 {
		return true
	}

	// Ignore everything else. Rationale:
	//
	//   * chmod: We don't really care about these as they won't affect build
	//     output. (Unless potentially we no longer can read the file, but
	//     we'll go down that path if it ever becomes a problem.)
	//
	//   * rename: Will produce a following create event as well, so just
	//     listen for that instead.
	//
	return false
}

// Replaces the current process with a fresh one by invoking the same
// executable with the operating system's exec syscall. This is prompted by the
// USR2 signal and is intended to allow the process to refresh itself in the
// case where it's source files changed and it was recompiled.
//
// The fsnotify watcher and HTTP server are shut down as gracefully as possible
// before the replacement occurs.
func shutdownAndExec(c *Context, finish chan struct{},
	watcher *fsnotify.Watcher, server *http.Server) {

	// Tell the build loop to finish up
	finish <- struct{}{}

	// DANGER: Defers don't seem to get called on the re-exec, so even though
	// we have a defer which closes our watcher, it won't close, leading to
	// file descriptor leaking. Close it manually here instead.
	watcher.Close()

	// A context that will act as a timeout for connections
	// that are still running as we try and shut down the HTTP
	// server.
	timeoutCtx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)
	defer cancel()

	c.Log.Infof("Shutting down HTTP server")
	if err := server.Shutdown(timeoutCtx); err != nil {
		exitWithError(err)
	}

	// Returns an absolute path.
	execPath, err := os.Executable()
	if err != nil {
		exitWithError(err)
	}

	c.Log.Infof("Execing process '%s' with args %+v\n", execPath, os.Args)
	if err := unix.Exec(execPath, os.Args, os.Environ()); err != nil {
		exitWithError(err)
	}
}

// Sorts a slice of jobs with the slowest on top.
func sortJobsBySlowest(jobs []*Job) {
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[j].Duration < jobs[i].Duration
	})
}

// Listens for file system changes from fsnotify and pushes relevant ones back
// out over the rebuild channel.
//
// It doesn't start listening to fsnotify again until the main loop has
// signaled rebuildDone, so there is a possibility that in the case of very
// fast consecutive changes the build might not be perfectly up to date.
func watchChanges(c *Context, watcher *fsnotify.Watcher,
	rebuild chan map[string]struct{}, rebuildDone chan struct{}) {

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			c.Log.Debugf("Received event from watcher: %+v", event)
			lastChangedSources := map[string]struct{}{event.Name: {}}

			if !shouldRebuild(event.Name, event.Op) {
				continue
			}

			// The central purpose of this loop is to make sure we do as few
			// build loops given incoming changes as possible.
			//
			// On the first receipt of a rebuild-eligible event we start
			// rebuilding immediately, and during the rebuild we accumulate any
			// other rebuild-eligible changes that stream in. When the initial
			// build finishes, we loop and start a new one.
			//
			// This process continues until a build complete and there
			for {
				if len(lastChangedSources) < 1 {
					break
				}

				// Start rebuild
				rebuild <- lastChangedSources

				// Zero out the last set of changes and start accumulating.
				lastChangedSources = nil

				// Wait until rebuild is finished. In the meantime, accumulate
				// new events that come in on the watcher's channel and prepare
				// for the next loop..
			INNER_LOOP:
				for {
					select {
					case <-rebuildDone:
						// Break and start next outer loop
						break INNER_LOOP

					case event := <-watcher.Events:
						if shouldRebuild(event.Name, event.Op) {
							if lastChangedSources == nil {
								lastChangedSources = make(map[string]struct{})
							}

							lastChangedSources[event.Name] = struct{}{}
						}
					}
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			c.Log.Errorf("Error from watcher:", err)
		}
	}
}