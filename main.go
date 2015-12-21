package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

// RepositorySummary is the model of our evaluation of a repository health.
type RepositorySummary struct {
	Repository github.Repository
	HasReadme  bool
}

// Job represents the job to be run. This is an abstraction to control how many
type Job struct {
	// Callable is the unit of work that should be wrapped.
	Callable func()
}

// Worker represents the worker that executes the job
type Worker struct {
	WorkerPool chan chan Job
	JobChannel chan Job
	quit       chan struct{}
}

// NewWorker creates a new Worker which wraps the provided workerPool.
func NewWorker(workerPool chan chan Job) Worker {
	return Worker{
		WorkerPool: workerPool,
		JobChannel: make(chan Job),
		quit:       make(chan struct{})}
}

// Start method starts the run loop for the worker, listening for a quit channel in
// case we need to stop it
func (w Worker) Start() {
	go func() {
		for {
			// register the current worker into the worker queue.
			w.WorkerPool <- w.JobChannel

			select {
			case job := <-w.JobChannel:
				// we have received a work request.
				// Do the work
				job.Callable()

			case <-w.quit:
				// we have received a signal to stop
				return
			}
		}
	}()
}

// Stop signals the worker to stop listening for work requests.
func (w Worker) Stop() {
	go func() {
		w.quit <- struct{}{}
	}()
}

// Dispatcher is a throttling mechanism so that we don't make too many requests to
// HTTP servers (and thus run out of file handles, slow down too much).
type Dispatcher struct {
	// A pool of workers channels that are registered with the dispatcher
	WorkerPool chan chan Job
	maxWorkers int
	jobQueue   chan Job
}

// NewDispatcher creates a Dispatcher with the specified number of workers.
func NewDispatcher(maxWorkers int, jobQueue chan Job) *Dispatcher {
	pool := make(chan chan Job, maxWorkers)
	return &Dispatcher{WorkerPool: pool,
		maxWorkers: maxWorkers,
		jobQueue:   jobQueue,
	}
}

// Run starts all the workers and runs the select loop to do work.
func (d *Dispatcher) Run() {
	// starting n number of workers
	for i := 0; i < d.maxWorkers; i++ {
		worker := NewWorker(d.WorkerPool)
		worker.Start()
	}

	go d.dispatch()
}

func (d *Dispatcher) dispatch() {
	for {
		select {
		case job := <-d.jobQueue:
			// a job request has been received
			// fmt.Printf("Received a job request\n")

			go func(job Job) {
				// try to obtain a worker job channel that is available.
				// this will block until a worker is idle
				// fmt.Printf("Retrieving a worker\n")
				jobChannel := <-d.WorkerPool

				// dispatch the job to the worker job channel
				// fmt.Printf("Dispatching the job to the worker\n")
				jobChannel <- job
			}(job)
		}
	}
}

func (rs RepositorySummary) String() string {
	return fmt.Sprintf("[name=%s, HasReadme=%v]", *rs.Repository.Name, rs.HasReadme)
}

func main() {
	oauthToken := os.Getenv("GH_OAUTH_TOKEN")

	if oauthToken == "" {
		os.Exit(1)
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: oauthToken},
	)
	tc := oauth2.NewClient(oauth2.NoContext, ts)

	client := github.NewClient(tc)

	org := os.Getenv("GH_ORG")

	if org == "" {
		os.Exit(1)
	}

	jobQueue := make(chan Job)
	dispatcher := NewDispatcher(10, jobQueue)
	dispatcher.Run()

	allRepos, err := getAllRepos(client, org)

	if err != nil {
		fmt.Printf("%v\n", err)
		return
	}

	// fmt.Printf("Got %v repositories\n", len(allRepos))

	in := repoChannel(allRepos)

	// For each repo, check that it has a README and description
	for summary := range merge(jobQueue, client, org, in) {
		fmt.Printf("%v\n", summary)
	}
}

func repoChannel(repos []github.Repository) <-chan github.Repository {
	out := make(chan github.Repository)
	go func() {
		for _, repo := range repos {
			out <- repo
		}
		close(out)
	}()
	return out
}

func merge(jobQueue chan Job, client *github.Client, org string, repos <-chan github.Repository) <-chan RepositorySummary {
	var wg sync.WaitGroup
	out := make(chan RepositorySummary)

	newSummary := func(client *github.Client, repo github.Repository) func() {
		return func() {
			defer wg.Done()
			// fmt.Printf("Retrieving README for %v\n", *repo.Name)
			readme, resp, err := client.Repositories.GetReadme(org, *repo.Name, nil)
			out <- RepositorySummary{Repository: repo,
				HasReadme: err == nil && readme != nil,
			}
			httpCleanup(resp)
		}
	}

	for repo := range repos {
		// fmt.Printf("Creating job for %v\n", *repo.Name)
		work := Job{Callable: newSummary(client, repo)}
		wg.Add(1)
		// fmt.Printf("Submitting job for %v\n", *repo.Name)
		jobQueue <- work
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

func getAllRepos(client *github.Client, org string) ([]github.Repository, error) {
	opt := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{PerPage: 40},
	}

	// get all pages of results
	var allRepos []github.Repository
	for {
		repos, resp, err := client.Repositories.ListByOrg(org, opt)
		if err != nil {
			return nil, err
		}
		allRepos = append(allRepos, repos...)
		if resp.NextPage == 0 {
			break
		}
		opt.ListOptions.Page = resp.NextPage
		httpCleanup(resp)
	}

	return allRepos, nil
}

func httpCleanup(resp *github.Response) {
	if resp == nil {
		return
	}

	if resp.Body == nil {
		return
	}

	resp.Body.Close()
}
