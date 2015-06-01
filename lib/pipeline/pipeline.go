// package pipeline implements a system for running data pipelines on top of
// the filesystem
package pipeline

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/fsouza/go-dockerclient"
	"github.com/pachyderm/pfs/lib/btrfs"
	"github.com/pachyderm/pfs/lib/container"
)

var Cancelled = errors.New("cancelled")

type Pipeline struct {
	config          docker.CreateContainerOptions
	inRepo, outRepo string
	commit, branch  string
	counter         int
	container       string
	cancelled       bool
	runWait         sync.WaitGroup
}

func NewPipeline(dataRepo, outRepo, commit, branch string) *Pipeline {
	return &Pipeline{
		inRepo:  dataRepo,
		outRepo: outRepo,
		commit:  commit,
		branch:  branch,
		config: docker.CreateContainerOptions{Config: &docker.Config{},
			HostConfig: &docker.HostConfig{}},
	}
}

// Import makes a dataset available for computations in the container.
func (p *Pipeline) Import(name string) error {
	hostPath := btrfs.HostPath(path.Join(p.inRepo, p.commit, name))
	containerPath := path.Join("/in", name)

	bind := fmt.Sprintf("%s:%s:ro", hostPath, containerPath)
	p.config.HostConfig.Binds = append(p.config.HostConfig.Binds, bind)
	return nil
}

// Image sets the image that is being used for computations.
func (p *Pipeline) Image(image string) error {
	p.config.Config.Image = image
	return nil
}

// Start gets an outRepo ready to be used. This is where clean up of dirty
// state from a crash happens.
func (p *Pipeline) Start() error {
	// If our branch in outRepo has the same parent as the commit in inRepo it
	// means the last run of the pipeline was succesful.
	parent := btrfs.GetMeta(path.Join(p.outRepo, p.branch), "parent")
	if parent != btrfs.GetMeta(path.Join(p.inRepo, p.commit), "parent") {
		return btrfs.DanglingCommit(p.outRepo, p.commit+"-pre", p.branch)
	}
	return nil
}

// runCommit returns the commit that the current run will create
func (p *Pipeline) runCommit() string {
	return fmt.Sprintf("%s-%d", p.commit, p.counter)
}

// Run runs a command in the container, it assumes that `branch` has already
// been created.
// Notice that any failure in this function leads to the branch having
// uncommitted dirty changes. This state needs to be cleaned up before the
// pipeline is rerun. The reason we don't do it here is that even if we try our
// best the process crashing at the wrong time could still leave it in an
// inconsistent state.
func (p *Pipeline) Run(cmd []string) error {
	// this function always increments counter
	defer func() { p.counter++ }()
	// Check if the commit already exists
	exists, err := btrfs.FileExists(path.Join(p.outRepo, p.runCommit()))
	if err != nil {
		log.Print(err)
		return err
	}
	// if the commit exists there's no work to be done
	if exists {
		return nil
	}
	// Set the command
	p.config.Config.Cmd = cmd
	// Map the out directory in as a bind
	hostPath := btrfs.HostPath(path.Join(p.outRepo, p.branch))
	bind := fmt.Sprintf("%s:/out", hostPath)
	p.config.HostConfig.Binds = append(p.config.HostConfig.Binds, bind)
	// Make sure this bind is only visible for the duration of run
	defer func() { p.config.HostConfig.Binds = p.config.HostConfig.Binds[:len(p.config.HostConfig.Binds)-1] }()
	// Start the container
	p.container, err = container.RawStartContainer(p.config)
	if err != nil {
		log.Print(err)
		return err
	}
	// Create a place to put the logs
	f, err := btrfs.CreateAll(path.Join(p.outRepo, p.branch, ".log"))
	if err != nil {
		log.Print(err)
		return err
	}
	defer f.Close()
	// Copy the logs from the container in to the file.
	go func() {
		err := container.ContainerLogs(p.container, f)
		if err != nil {
			log.Print(err)
		}
	}()
	// Wait for the command to finish:
	exit, err := container.WaitContainer(p.container)
	if err != nil {
		log.Print(err)
		return err
	}
	if exit != 0 {
		// The command errored
		return fmt.Errorf("Command:\n\t%s\nhad exit code: %d.\n",
			strings.Join(cmd, " "), exit)
	}
	// Commit the results
	err = btrfs.Commit(p.outRepo, p.runCommit(), p.branch)
	if err != nil {
		log.Print(err)
		return err
	}
	return nil
}

// Finish makes the final commit for the pipeline
func (p *Pipeline) Finish() error {
	exists, err := btrfs.FileExists(path.Join(p.outRepo, p.commit))
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return btrfs.Commit(p.outRepo, p.commit, p.branch)
}

// Cancel stops a pipeline by force before it's finished
func (p *Pipeline) Cancel() error {
	p.cancelled = true
	err := container.StopContainer(p.container)
	if err != nil {
		log.Print(err)
		return err
	}
	return nil
}

func (p *Pipeline) RunPachFile(r io.Reader) error {
	lines := bufio.NewScanner(r)

	if err := p.Start(); err != nil {
		return err
	}
	for lines.Scan() {
		if p.cancelled {
			return Cancelled
		}
		tokens := strings.Fields(lines.Text())
		if len(tokens) < 2 {
			continue
		}

		var err error
		switch strings.ToLower(tokens[0]) {
		case "import":
			err = p.Import(tokens[1])
		case "image":
			err = p.Image(tokens[1])
		case "run":
			err = p.Run(tokens[1:])
		}
		if err != nil {
			log.Print(err)
			return err
		}
	}
	if err := p.Finish(); err != nil {
		return err
	}
	return nil
}

type Runner struct {
	pipelineDir, inRepo, commit, branch string
	outPrefix                           string // the prefix for out repos
	pipelines                           []*Pipeline
	wait                                sync.WaitGroup
	lock                                sync.Mutex // used to prevent races between `Run` and `Cancel`
	cancelled                           bool
}

func NewRunner(pipelineDir, inRepo, outPrefix, commit, branch string) *Runner {
	return &Runner{
		pipelineDir: pipelineDir,
		inRepo:      inRepo,
		outPrefix:   outPrefix,
		commit:      commit,
		branch:      branch,
	}
}

func (r *Runner) makeOutRepo(pipeline string) error {
	err := btrfs.Ensure(path.Join(r.outPrefix, pipeline))
	if err != nil {
		log.Print(err)
		return err
	}

	exists, err := btrfs.FileExists(path.Join(r.outPrefix, pipeline, r.branch))
	if err != nil {
		log.Print(err)
		return err
	}
	if !exists {
		// The branch doesn't exist, we need to create it We'll make our branch
		// have the same parent as the commit we're running off of if that
		// parent exists in the pipelines outRepo. This lets us carry over past
		// computation results when a new branch is created rather than having
		// to start from scratch.
		parent := btrfs.GetMeta(path.Join(r.inRepo, r.commit), "parent")
		if parent != "" {
			exists, err := btrfs.FileExists(path.Join(r.outPrefix, pipeline, parent))
			if err != nil {
				log.Print(err)
				return err
			}
			if !exists {
				parent = ""
			}
		}
		err := btrfs.Branch(path.Join(r.outPrefix, pipeline), parent, r.branch)
		if err != nil {
			log.Print(err)
			return err
		}
	}
	// The branch exists, so we're ready to return
	return nil
}

// Run runs all of the pipelines it finds in pipelineDir. Returns the
// first error it encounters.
func (r *Runner) Run() error {
	err := btrfs.MkdirAll(r.outPrefix)
	if err != nil {
		return err
	}
	pipelines, err := btrfs.ReadDir(path.Join(r.inRepo, r.commit, r.pipelineDir))
	if err != nil {
		return err
	}
	// A chanel for the errors, notice that it's capacity is the same as the
	// number of pipelines. The below code should make sure that each pipeline only
	// sends 1 error otherwise deadlock may occur.
	errors := make(chan error, len(pipelines))

	// Make sure we don't race with cancel this is held while we add pipelines.
	r.lock.Lock()
	if r.cancelled {
		// we were cancelled before we even started
		r.lock.Unlock()
		return Cancelled
	}
	r.wait.Add(len(pipelines))
	for _, pInfo := range pipelines {
		err := r.makeOutRepo(pInfo.Name())
		if err != nil {
			log.Print(err)
			return err
		}
		p := NewPipeline(r.inRepo, path.Join(r.outPrefix, pInfo.Name()), r.commit, r.branch)
		r.pipelines = append(r.pipelines, p)
		go func(pInfo os.FileInfo, p *Pipeline) {
			defer r.wait.Done()
			f, err := btrfs.Open(path.Join(r.inRepo, r.commit, r.pipelineDir, pInfo.Name()))
			if err != nil {
				log.Print(err)
				errors <- err
				return
			}
			err = p.RunPachFile(f)
			if err != nil {
				log.Print(err)
				errors <- err
				return
			}
		}(pInfo, p)
	}
	// We're done adding pipelines so unlock
	r.lock.Unlock()
	// Wait for the pipelines to finish
	r.wait.Wait()
	close(errors)
	if r.cancelled {
		// Pipelines finished because we were cancelled
		return Cancelled
	}
	for err := range errors {
		return err
	}
	return nil
}

// RunPipelines lets you easily run the Pipelines in one line if you don't care about cancelling them.
func RunPipelines(pipelineDir, inRepo, outRepo, commit, branch string) error {
	return NewRunner(pipelineDir, inRepo, outRepo, commit, branch).Run()
}

func (r *Runner) Cancel() error {
	// A chanel for the errors, notice that it's capacity is the same as the
	// number of pipelines. The below code should make sure that each pipeline only
	// sends 1 error otherwise deadlock may occur.
	errors := make(chan error, len(r.pipelines))

	// Make sure we don't race with Run
	r.lock.Lock()
	// Indicate that we're cancelling the pipelines
	r.cancelled = true
	// A waitgroup for the goros that cancel the containers
	var wg sync.WaitGroup
	// We'll have one goro per pipelines
	wg.Add(len(r.pipelines))
	for _, p := range r.pipelines {
		go func(p *Pipeline) {
			defer wg.Done()
			err := p.Cancel()
			if err != nil {
				errors <- err
			}
		}(p)
	}
	// Wait for the cancellations to finish.
	wg.Wait()
	r.lock.Unlock()
	close(errors)
	for err := range errors {
		return err
	}
	// At the end we wait for the pipelines to actually finish, this means that
	// once Cancel is done you can safely fire off a new bathc of pipelines.
	r.wait.Wait()
	return nil
}
